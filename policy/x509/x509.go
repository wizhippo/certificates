package x509policy

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	"go.step.sm/crypto/x509util"
)

type CertificateInvalidError struct {
	Reason x509.InvalidReason
	Detail string
}

func (e CertificateInvalidError) Error() string {
	switch e.Reason {
	// TODO: include logical errors for this package; exlude ones that don't make sense for its current use case?
	// TODO: currently only CANotAuthorizedForThisName is used by this package; we're not checking the other things in CSRs in this package.
	case x509.NotAuthorizedToSign:
		return "not authorized to sign other certificates" // TODO: this one doesn't make sense for this pkg
	case x509.Expired:
		return "csr has expired or is not yet valid: " + e.Detail
	case x509.CANotAuthorizedForThisName:
		return "not authorized to sign for this name: " + e.Detail
	case x509.CANotAuthorizedForExtKeyUsage:
		return "not authorized for an extended key usage: " + e.Detail
	case x509.TooManyIntermediates:
		return "too many intermediates for path length constraint"
	case x509.IncompatibleUsage:
		return "csr specifies an incompatible key usage"
	case x509.NameMismatch:
		return "issuer name does not match subject from issuing certificate"
	case x509.NameConstraintsWithoutSANs:
		return "issuer has name constraints but csr doesn't have a SAN extension"
	case x509.UnconstrainedName:
		return "issuer has name constraints but csr contains unknown or unconstrained name: " + e.Detail
	}
	return "unknown error"
}

// NamePolicyEngine can be used to check that a CSR or Certificate meets all allowed and
// denied names before a CA creates and/or signs the Certificate.
// TODO(hs): the x509 RFC also defines name checks on directory name; support that?
// TODO(hs): implement Stringer interface: describe the contents of the NamePolicyEngine?
type NamePolicyEngine struct {
	options                 []NamePolicyOption
	permittedDNSDomains     []string
	excludedDNSDomains      []string
	permittedIPRanges       []*net.IPNet
	excludedIPRanges        []*net.IPNet
	permittedEmailAddresses []string
	excludedEmailAddresses  []string
	permittedURIDomains     []string
	excludedURIDomains      []string
}

// NewNamePolicyEngine creates a new NamePolicyEngine with NamePolicyOptions
func New(opts ...NamePolicyOption) (*NamePolicyEngine, error) {

	e := &NamePolicyEngine{}
	e.options = append(e.options, opts...)
	for _, option := range e.options {
		if err := option(e); err != nil {
			return nil, err
		}
	}

	return e, nil
}

// AreCertificateNamesAllowed verifies that all SANs in a Certificate are allowed.
func (e *NamePolicyEngine) AreCertificateNamesAllowed(cert *x509.Certificate) (bool, error) {
	if err := e.validateNames(cert.DNSNames, cert.IPAddresses, cert.EmailAddresses, cert.URIs); err != nil {
		return false, err
	}
	return true, nil
}

// AreCSRNamesAllowed verifies that all names in the CSR are allowed.
func (e *NamePolicyEngine) AreCSRNamesAllowed(csr *x509.CertificateRequest) (bool, error) {
	if err := e.validateNames(csr.DNSNames, csr.IPAddresses, csr.EmailAddresses, csr.URIs); err != nil {
		return false, err
	}
	return true, nil
}

// AreSANSAllowed verifies that all names in the slice of SANs are allowed.
// The SANs are first split into DNS names, IPs, email addresses and URIs.
func (e *NamePolicyEngine) AreSANsAllowed(sans []string) (bool, error) {
	dnsNames, ips, emails, uris := x509util.SplitSANs(sans)
	if err := e.validateNames(dnsNames, ips, emails, uris); err != nil {
		return false, err
	}
	return true, nil
}

// IsDNSAllowed verifies a single DNS domain is allowed.
func (e *NamePolicyEngine) IsDNSAllowed(dns string) (bool, error) {
	if err := e.validateNames([]string{dns}, []net.IP{}, []string{}, []*url.URL{}); err != nil {
		return false, err
	}
	return true, nil
}

// IsIPAllowed verifies a single IP domain is allowed.
func (e *NamePolicyEngine) IsIPAllowed(ip net.IP) (bool, error) {
	if err := e.validateNames([]string{}, []net.IP{ip}, []string{}, []*url.URL{}); err != nil {
		return false, err
	}
	return true, nil
}

// validateNames verifies that all names are allowed.
// Its logic follows that of (a large part of) the (c *Certificate) isValid() function
// in https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
func (e *NamePolicyEngine) validateNames(dnsNames []string, ips []net.IP, emailAddresses []string, uris []*url.URL) error {

	// TODO: return our own type of error?

	// TODO: set limit on total of all names? In x509 there's a limit on the number of comparisons
	// that protects the CA from a DoS (i.e. many heavy comparisons). The x509 implementation takes
	// this number as a total of all checks and keeps a (pointer to a) counter of the number of checks
	// executed so far.

	// TODO: gather all errors, or return early? Currently we return early on the first wrong name; check might fail for multiple names.
	// Perhaps make that an option?
	for _, dns := range dnsNames {
		if _, ok := domainToReverseLabels(dns); !ok {
			return errors.Errorf("cannot parse dns %q", dns)
		}
		if err := checkNameConstraints("dns", dns, dns,
			func(parsedName, constraint interface{}) (bool, error) {
				return matchDomainConstraint(parsedName.(string), constraint.(string))
			}, e.permittedDNSDomains, e.excludedDNSDomains); err != nil {
			return err
		}
	}

	for _, ip := range ips {
		if err := checkNameConstraints("ip", ip.String(), ip,
			func(parsedName, constraint interface{}) (bool, error) {
				return matchIPConstraint(parsedName.(net.IP), constraint.(*net.IPNet))
			}, e.permittedIPRanges, e.excludedIPRanges); err != nil {
			return err
		}
	}

	for _, email := range emailAddresses {
		mailbox, ok := parseRFC2821Mailbox(email)
		if !ok {
			return fmt.Errorf("cannot parse rfc822Name %q", mailbox)
		}
		if err := checkNameConstraints("email", email, mailbox,
			func(parsedName, constraint interface{}) (bool, error) {
				return matchEmailConstraint(parsedName.(rfc2821Mailbox), constraint.(string))
			}, e.permittedEmailAddresses, e.excludedEmailAddresses); err != nil {
			return err
		}
	}

	for _, uri := range uris {
		if err := checkNameConstraints("uri", uri.String(), uri,
			func(parsedName, constraint interface{}) (bool, error) {
				return matchURIConstraint(parsedName.(*url.URL), constraint.(string))
			}, e.permittedURIDomains, e.excludedURIDomains); err != nil {
			return err
		}
	}

	// TODO: when the error is not nil and returned up in the above, we can add
	// additional context to it (i.e. the cert or csr that was inspected).

	// TODO(hs): validate other types of SANs? The Go std library skips those.
	// These could be custom checkers.

	// if all checks out, all SANs are allowed
	return nil
}

// checkNameConstraints checks that c permits a child certificate to claim the
// given name, of type nameType. The argument parsedName contains the parsed
// form of name, suitable for passing to the match function. The total number
// of comparisons is tracked in the given count and should not exceed the given
// limit.
// SOURCE: https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
func checkNameConstraints(
	nameType string,
	name string,
	parsedName interface{},
	match func(parsedName, constraint interface{}) (match bool, err error),
	permitted, excluded interface{}) error {

	excludedValue := reflect.ValueOf(excluded)

	// *count += excludedValue.Len()
	// if *count > maxConstraintComparisons {
	// 	return x509.CertificateInvalidError{c, x509.TooManyConstraints, ""}
	// }

	// TODO: fix the errors; return our own, because we don't have cert ...

	for i := 0; i < excludedValue.Len(); i++ {
		constraint := excludedValue.Index(i).Interface()
		match, err := match(parsedName, constraint)
		if err != nil {
			return CertificateInvalidError{
				Reason: x509.CANotAuthorizedForThisName,
				Detail: err.Error(),
			}
		}

		if match {
			return CertificateInvalidError{
				Reason: x509.CANotAuthorizedForThisName,
				Detail: fmt.Sprintf("%s %q is excluded by constraint %q", nameType, name, constraint),
			}
		}
	}

	permittedValue := reflect.ValueOf(permitted)

	// *count += permittedValue.Len()
	// if *count > maxConstraintComparisons {
	// 	return x509.CertificateInvalidError{c, x509.TooManyConstraints, ""}
	// }

	ok := true
	for i := 0; i < permittedValue.Len(); i++ {
		constraint := permittedValue.Index(i).Interface()
		var err error
		if ok, err = match(parsedName, constraint); err != nil {
			return CertificateInvalidError{
				Reason: x509.CANotAuthorizedForThisName,
				Detail: err.Error(),
			}
		}

		if ok {
			break
		}
	}

	if !ok {
		return CertificateInvalidError{
			Reason: x509.CANotAuthorizedForThisName,
			Detail: fmt.Sprintf("%s %q is not permitted by any constraint", nameType, name),
		}
	}

	return nil
}

// domainToReverseLabels converts a textual domain name like foo.example.com to
// the list of labels in reverse order, e.g. ["com", "example", "foo"].
// SOURCE: https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
func domainToReverseLabels(domain string) (reverseLabels []string, ok bool) {
	for len(domain) > 0 {
		if i := strings.LastIndexByte(domain, '.'); i == -1 {
			reverseLabels = append(reverseLabels, domain)
			domain = ""
		} else {
			reverseLabels = append(reverseLabels, domain[i+1:])
			domain = domain[:i]
		}
	}

	if len(reverseLabels) > 0 && reverseLabels[0] == "" {
		// An empty label at the end indicates an absolute value.
		return nil, false
	}

	for _, label := range reverseLabels {
		if label == "" {
			// Empty labels are otherwise invalid.
			return nil, false
		}

		for _, c := range label {
			if c < 33 || c > 126 {
				// Invalid character.
				return nil, false
			}
		}
	}

	return reverseLabels, true
}

// rfc2821Mailbox represents a “mailbox” (which is an email address to most
// people) by breaking it into the “local” (i.e. before the '@') and “domain”
// parts.
// SOURCE: https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
type rfc2821Mailbox struct {
	local, domain string
}

// parseRFC2821Mailbox parses an email address into local and domain parts,
// based on the ABNF for a “Mailbox” from RFC 2821. According to RFC 5280,
// Section 4.2.1.6 that's correct for an rfc822Name from a certificate: “The
// format of an rfc822Name is a "Mailbox" as defined in RFC 2821, Section 4.1.2”.
// SOURCE: https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
func parseRFC2821Mailbox(in string) (mailbox rfc2821Mailbox, ok bool) {
	if in == "" {
		return mailbox, false
	}

	localPartBytes := make([]byte, 0, len(in)/2)

	if in[0] == '"' {
		// Quoted-string = DQUOTE *qcontent DQUOTE
		// non-whitespace-control = %d1-8 / %d11 / %d12 / %d14-31 / %d127
		// qcontent = qtext / quoted-pair
		// qtext = non-whitespace-control /
		//         %d33 / %d35-91 / %d93-126
		// quoted-pair = ("\" text) / obs-qp
		// text = %d1-9 / %d11 / %d12 / %d14-127 / obs-text
		//
		// (Names beginning with “obs-” are the obsolete syntax from RFC 2822,
		// Section 4. Since it has been 16 years, we no longer accept that.)
		in = in[1:]
	QuotedString:
		for {
			if in == "" {
				return mailbox, false
			}
			c := in[0]
			in = in[1:]

			switch {
			case c == '"':
				break QuotedString

			case c == '\\':
				// quoted-pair
				if in == "" {
					return mailbox, false
				}
				if in[0] == 11 ||
					in[0] == 12 ||
					(1 <= in[0] && in[0] <= 9) ||
					(14 <= in[0] && in[0] <= 127) {
					localPartBytes = append(localPartBytes, in[0])
					in = in[1:]
				} else {
					return mailbox, false
				}

			case c == 11 ||
				c == 12 ||
				// Space (char 32) is not allowed based on the
				// BNF, but RFC 3696 gives an example that
				// assumes that it is. Several “verified”
				// errata continue to argue about this point.
				// We choose to accept it.
				c == 32 ||
				c == 33 ||
				c == 127 ||
				(1 <= c && c <= 8) ||
				(14 <= c && c <= 31) ||
				(35 <= c && c <= 91) ||
				(93 <= c && c <= 126):
				// qtext
				localPartBytes = append(localPartBytes, c)

			default:
				return mailbox, false
			}
		}
	} else {
		// Atom ("." Atom)*
	NextChar:
		for len(in) > 0 {
			// atext from RFC 2822, Section 3.2.4
			c := in[0]

			switch {
			case c == '\\':
				// Examples given in RFC 3696 suggest that
				// escaped characters can appear outside of a
				// quoted string. Several “verified” errata
				// continue to argue the point. We choose to
				// accept it.
				in = in[1:]
				if in == "" {
					return mailbox, false
				}
				fallthrough

			case ('0' <= c && c <= '9') ||
				('a' <= c && c <= 'z') ||
				('A' <= c && c <= 'Z') ||
				c == '!' || c == '#' || c == '$' || c == '%' ||
				c == '&' || c == '\'' || c == '*' || c == '+' ||
				c == '-' || c == '/' || c == '=' || c == '?' ||
				c == '^' || c == '_' || c == '`' || c == '{' ||
				c == '|' || c == '}' || c == '~' || c == '.':
				localPartBytes = append(localPartBytes, in[0])
				in = in[1:]

			default:
				break NextChar
			}
		}

		if len(localPartBytes) == 0 {
			return mailbox, false
		}

		// From RFC 3696, Section 3:
		// “period (".") may also appear, but may not be used to start
		// or end the local part, nor may two or more consecutive
		// periods appear.”
		twoDots := []byte{'.', '.'}
		if localPartBytes[0] == '.' ||
			localPartBytes[len(localPartBytes)-1] == '.' ||
			bytes.Contains(localPartBytes, twoDots) {
			return mailbox, false
		}
	}

	if in == "" || in[0] != '@' {
		return mailbox, false
	}
	in = in[1:]

	// The RFC species a format for domains, but that's known to be
	// violated in practice so we accept that anything after an '@' is the
	// domain part.
	if _, ok := domainToReverseLabels(in); !ok {
		return mailbox, false
	}

	mailbox.local = string(localPartBytes)
	mailbox.domain = in
	return mailbox, true
}

// SOURCE: https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
func matchDomainConstraint(domain, constraint string) (bool, error) {
	// The meaning of zero length constraints is not specified, but this
	// code follows NSS and accepts them as matching everything.
	if constraint == "" {
		return true, nil
	}

	domainLabels, ok := domainToReverseLabels(domain)
	if !ok {
		return false, fmt.Errorf("cannot parse domain %q", domain)
	}

	// RFC 5280 says that a leading period in a domain name means that at
	// least one label must be prepended, but only for URI and email
	// constraints, not DNS constraints. The code also supports that
	// behavior for DNS constraints.

	mustHaveSubdomains := false
	if constraint[0] == '.' {
		mustHaveSubdomains = true
		constraint = constraint[1:]
	}

	constraintLabels, ok := domainToReverseLabels(constraint)
	if !ok {
		return false, fmt.Errorf("cannot parse domain %q", constraint)
	}

	if len(domainLabels) < len(constraintLabels) ||
		(mustHaveSubdomains && len(domainLabels) == len(constraintLabels)) {
		return false, nil
	}

	for i, constraintLabel := range constraintLabels {
		if !strings.EqualFold(constraintLabel, domainLabels[i]) {
			return false, nil
		}
	}

	return true, nil
}

// SOURCE: https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
func matchIPConstraint(ip net.IP, constraint *net.IPNet) (bool, error) {

	// TODO(hs): this is code from Go library, but I got some unexpected result:
	// with permitted net 127.0.0.0/24, 127.0.0.1 is NOT allowed. When parsing 127.0.0.1 as net.IP
	// which is in the IPAddresses slice, the underlying length is 16. The contraint.IP has a length
	// of 4 instead. I currently don't believe that this is a bug in Go now, but why is it like that?
	// Is there a difference because we're not operating on a sans []string slice? Or is the Go
	// implementation stricter regarding IPv4 vs. IPv6? I've been bitten by some unfortunate differences
	// between the two before (i.e. IPv4 in IPv6; IP SANS in ACME)
	// if len(ip) != len(constraint.IP) {
	// 	return false, nil
	// }

	// for i := range ip {
	// 	if mask := constraint.Mask[i]; ip[i]&mask != constraint.IP[i]&mask {
	// 		return false, nil
	// 	}
	// }

	// if isIPv4(ip) != isIPv4(constraint.IP) { // TODO(hs): this check seems to do what the above intended to do?
	// 	return false, nil
	// }

	contained := constraint.Contains(ip) // TODO(hs): validate that this is the correct behavior.

	return contained, nil
}

func isIPv4(ip net.IP) bool {
	return ip.To4() != nil
}

// SOURCE: https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
func matchEmailConstraint(mailbox rfc2821Mailbox, constraint string) (bool, error) {
	// If the constraint contains an @, then it specifies an exact mailbox name.
	if strings.Contains(constraint, "@") {
		constraintMailbox, ok := parseRFC2821Mailbox(constraint)
		if !ok {
			return false, fmt.Errorf("cannot parse constraint %q", constraint)
		}
		return mailbox.local == constraintMailbox.local && strings.EqualFold(mailbox.domain, constraintMailbox.domain), nil
	}

	// Otherwise the constraint is like a DNS constraint of the domain part
	// of the mailbox.
	return matchDomainConstraint(mailbox.domain, constraint)
}

// SOURCE: https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/crypto/x509/verify.go
func matchURIConstraint(uri *url.URL, constraint string) (bool, error) {
	// From RFC 5280, Section 4.2.1.10:
	// “a uniformResourceIdentifier that does not include an authority
	// component with a host name specified as a fully qualified domain
	// name (e.g., if the URI either does not include an authority
	// component or includes an authority component in which the host name
	// is specified as an IP address), then the application MUST reject the
	// certificate.”

	host := uri.Host
	if host == "" {
		return false, fmt.Errorf("URI with empty host (%q) cannot be matched against constraints", uri.String())
	}

	if strings.Contains(host, ":") && !strings.HasSuffix(host, "]") {
		var err error
		host, _, err = net.SplitHostPort(uri.Host)
		if err != nil {
			return false, err
		}
	}

	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") ||
		net.ParseIP(host) != nil {
		return false, fmt.Errorf("URI with IP (%q) cannot be matched against constraints", uri.String())
	}

	return matchDomainConstraint(host, constraint)
}
