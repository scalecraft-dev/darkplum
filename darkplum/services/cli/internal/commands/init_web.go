package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The install's private web address: https://darkplum.<domain> (or
// --hostname), served by an internal NLB that Terraform creates
// (terraform/aws/modules/web-access), with a certificate and DNS name in a
// public zone that resolves to private addresses. init asks for the domain,
// checks the choices read-only against the account, and writes the web_*
// variables. It creates one thing itself, and only when the domain's DNS is
// not in this account: a hosted zone for the hostname, which the customer
// delegates from their DNS provider before Terraform issues the certificate.

// defaultVPCCIDR is variables.tf's vpc_cidr default; init does not write vpc_cidr.
const defaultVPCCIDR = "10.0.0.0/16"

// webSettings is the private-address part of the interview.
type webSettings struct {
	domain, hostname string
	zoneID, certARN  string
	allowed          []string
	createZone       bool // init creates the zone (domain's DNS is elsewhere)
	nameServers      []string
}

func (w webSettings) defaultHostname() bool { return w.hostname == "darkplum."+w.domain }

func (w webSettings) kvs() []tfvarsKV {
	if w.domain == "" {
		return nil
	}
	kv := []tfvarsKV{{"web_domain", w.domain}}
	if !w.defaultHostname() {
		kv = append(kv, tfvarsKV{"web_hostname", w.hostname})
	}
	if w.zoneID != "" {
		kv = append(kv, tfvarsKV{"web_zone_id", w.zoneID})
	}
	if w.certARN != "" {
		kv = append(kv, tfvarsKV{"web_certificate_arn", w.certARN})
	}
	return append(kv, tfvarsKV{"web_allowed_cidrs", w.allowed})
}

// --- validation (mirrors variables.tf; both are driven by variable_cases.json)

var dnsNameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

func isDNSName(s string) bool { return len(s) <= 253 && dnsNameRe.MatchString(s) }

// normalizeDNSName is what a person types turned into what is stored: lower
// case, no surrounding space, no trailing dot.
func normalizeDNSName(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

func validateTFWebDomain(d string) error {
	if d != "" && !isDNSName(d) {
		return errors.New("must be a lowercase DNS name such as acme.com")
	}
	return nil
}

func validateTFWebHostname(domain, h string) error {
	if h == "" {
		return nil
	}
	if domain == "" || !isDNSName(h) || !strings.HasSuffix(h, "."+domain) {
		return fmt.Errorf("must be a name inside %s, such as darkplum.%s", domain, domain)
	}
	return nil
}

var zoneIDRe = regexp.MustCompile(`^Z[A-Z0-9]{1,31}$`)

func validateTFWebZoneID(domain, id string) error {
	if id != "" && (domain == "" || !zoneIDRe.MatchString(id)) {
		return errors.New("must be a Route53 hosted zone ID such as Z0123456789ABCDEFGHIJ (and needs a domain)")
	}
	return nil
}

var certARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:acm:([a-z0-9-]+):([0-9]{12}):certificate/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validateTFWebCertARN(domain, region, arn string) error {
	if arn == "" {
		return nil
	}
	m := certARNRe.FindStringSubmatch(arn)
	if domain == "" || m == nil || m[1] != region {
		return fmt.Errorf("must be an ACM certificate ARN in %s (and needs a domain)", region)
	}
	return nil
}

// privateBlocks are RFC 1918: the only networks allowed to reach the address.
var privateBlocks = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// validateCIDR accepts an IPv4 network in canonical form that lies entirely
// inside RFC 1918 (so neither 0.0.0.0/0 nor a 0.0.0.0/1 + 128.0.0.0/1 split
// can open the address to the internet).
func validateCIDR(c string) error {
	ip, n, err := net.ParseCIDR(c)
	if err != nil || ip.To4() == nil || strings.Contains(c, ":") {
		return fmt.Errorf("%q is not an IPv4 network such as 10.8.0.0/16", c)
	}
	if !ip.Equal(n.IP) {
		return fmt.Errorf("%q is not in canonical form: did you mean %s?", c, n.String())
	}
	ones, _ := n.Mask.Size()
	for _, b := range privateBlocks {
		_, pb, _ := net.ParseCIDR(b)
		if bones, _ := pb.Mask.Size(); ones >= bones && pb.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("%q is not a private network (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16); the web address is private", c)
}

func validateTFWebCIDRs(domain string, cs []string) error {
	if len(cs) == 0 {
		return nil
	}
	if domain == "" {
		return errors.New("needs a domain")
	}
	for _, c := range cs {
		if err := validateCIDR(c); err != nil {
			return err
		}
	}
	return nil
}

func splitCIDRs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validateCIDRList(s string) error {
	cs := splitCIDRs(s)
	if len(cs) == 0 {
		return errors.New("enter at least one network, such as 10.0.0.0/16")
	}
	for _, c := range cs {
		if err := validateCIDR(c); err != nil {
			return err
		}
	}
	return nil
}

// certCovers reports whether a certificate for names serves host: an exact
// name, or a wildcard for exactly one label (*.acme.com covers x.acme.com, not
// acme.com nor a.b.acme.com). Case and a trailing dot do not matter.
func certCovers(names []string, host string) bool {
	host = normalizeDNSName(host)
	first, rest, ok := strings.Cut(host, ".")
	for _, n := range names {
		n = normalizeDNSName(n)
		if n == host {
			return true
		}
		if ok && first != "" && strings.HasPrefix(n, "*.") && n[2:] == rest {
			return true
		}
	}
	return false
}

// --- AWS (read-only, except createWebZone) ------------------------------------

type hostedZone struct {
	ID          string
	Name        string // no trailing dot
	Private     bool
	NameServers []string
}

type certInfo struct {
	Status   string
	NotAfter time.Time
	Names    []string // DomainName + SubjectAlternativeNames
}

func trimZoneID(id string) string { return strings.TrimPrefix(id, "/hostedzone/") }

// PublicZone finds this account's public hosted zone named name.
func (a *awsCloud) PublicZone(name string) (hostedZone, bool, error) {
	out, err := a.aws("route53", "list-hosted-zones-by-name", "--dns-name", name, "--max-items", "20", "--output", "json")
	if err != nil {
		return hostedZone{}, false, err
	}
	var r struct {
		HostedZones []struct {
			ID     string `json:"Id"`
			Name   string `json:"Name"`
			Config struct {
				PrivateZone bool `json:"PrivateZone"`
			} `json:"Config"`
		} `json:"HostedZones"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return hostedZone{}, false, errors.New("unexpected list-hosted-zones-by-name output")
	}
	for _, z := range r.HostedZones {
		if normalizeDNSName(z.Name) == name && !z.Config.PrivateZone {
			return hostedZone{ID: trimZoneID(z.ID), Name: name}, true, nil
		}
	}
	return hostedZone{}, false, nil
}

// HostedZone describes a zone by ID.
func (a *awsCloud) HostedZone(id string) (hostedZone, error) {
	out, err := a.aws("route53", "get-hosted-zone", "--id", id, "--output", "json")
	if err != nil {
		return hostedZone{}, err
	}
	var r struct {
		HostedZone struct {
			ID     string `json:"Id"`
			Name   string `json:"Name"`
			Config struct {
				PrivateZone bool `json:"PrivateZone"`
			} `json:"Config"`
		} `json:"HostedZone"`
		DelegationSet struct {
			NameServers []string `json:"NameServers"`
		} `json:"DelegationSet"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return hostedZone{}, errors.New("unexpected get-hosted-zone output")
	}
	return hostedZone{ID: trimZoneID(r.HostedZone.ID), Name: normalizeDNSName(r.HostedZone.Name),
		Private: r.HostedZone.Config.PrivateZone, NameServers: r.DelegationSet.NameServers}, nil
}

// RecordTypes lists the record types zoneID holds at exactly name.
func (a *awsCloud) RecordTypes(zoneID, name string) ([]string, error) {
	out, err := a.aws("route53", "list-resource-record-sets", "--hosted-zone-id", zoneID,
		"--start-record-name", name, "--max-items", "10", "--output", "json")
	if err != nil {
		return nil, err
	}
	var r struct {
		ResourceRecordSets []struct {
			Name string `json:"Name"`
			Type string `json:"Type"`
		} `json:"ResourceRecordSets"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, errors.New("unexpected list-resource-record-sets output")
	}
	var types []string
	for _, rr := range r.ResourceRecordSets {
		if normalizeDNSName(strings.ReplaceAll(rr.Name, `\052`, "*")) == name {
			types = append(types, rr.Type)
		}
	}
	return types, nil
}

// Certificate describes an ACM certificate in the ARN's own region.
func (a *awsCloud) Certificate(arn string) (certInfo, error) {
	region := ""
	if m := certARNRe.FindStringSubmatch(arn); m != nil {
		region = m[1]
	}
	out, err := a.aws("acm", "describe-certificate", "--certificate-arn", arn, "--region", region, "--output", "json")
	if err != nil {
		return certInfo{}, err
	}
	var r struct {
		Certificate struct {
			DomainName              string          `json:"DomainName"`
			SubjectAlternativeNames []string        `json:"SubjectAlternativeNames"`
			Status                  string          `json:"Status"`
			NotAfter                json.RawMessage `json:"NotAfter"`
		} `json:"Certificate"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return certInfo{}, errors.New("unexpected describe-certificate output")
	}
	ci := certInfo{Status: r.Certificate.Status, Names: append([]string{r.Certificate.DomainName}, r.Certificate.SubjectAlternativeNames...)}
	ci.NotAfter = parseAWSTime(r.Certificate.NotAfter)
	return ci, nil
}

// parseAWSTime reads the AWS CLI's timestamp: ISO 8601 by default, epoch
// seconds with cli_timestamp_format = none.
func parseAWSTime(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return time.Unix(int64(f), 0)
		}
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return time.Unix(int64(f), 0)
	}
	return time.Time{}
}

// CreateWebZone creates the public zone for hostname, or returns the one an
// earlier run created (so re-running init resumes rather than duplicating).
func (a *awsCloud) CreateWebZone(hostname string) (hostedZone, error) {
	if z, ok, err := a.PublicZone(hostname); err != nil {
		return hostedZone{}, err
	} else if ok {
		return a.HostedZone(z.ID)
	}
	out, err := a.aws("route53", "create-hosted-zone", "--name", hostname,
		"--caller-reference", fmt.Sprintf("darkplum-init-%s-%d", hostname, time.Now().Unix()),
		"--hosted-zone-config", "Comment=darkplum: private web address; delegate it from your DNS provider,PrivateZone=false",
		"--output", "json")
	if err != nil {
		return hostedZone{}, err
	}
	var r struct {
		HostedZone struct {
			ID string `json:"Id"`
		} `json:"HostedZone"`
		DelegationSet struct {
			NameServers []string `json:"NameServers"`
		} `json:"DelegationSet"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return hostedZone{}, errors.New("unexpected create-hosted-zone output")
	}
	return hostedZone{ID: trimZoneID(r.HostedZone.ID), Name: hostname, NameServers: r.DelegationSet.NameServers}, nil
}

// lookupNS is the public DNS answer for name's name servers.
func lookupNS(name string) ([]string, error) {
	ns, err := net.LookupNS(name)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Host
	}
	return out, nil
}

// delegated: public DNS answers name with every one of want's name servers.
func delegated(got, want []string) bool {
	if len(got) == 0 || len(want) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, g := range got {
		have[normalizeDNSName(g)] = true
	}
	for _, w := range want {
		if !have[normalizeDNSName(w)] {
			return false
		}
	}
	return true
}

// --- the interview -----------------------------------------------------------

// chooseWeb asks for (or takes from flags) the private address settings and
// checks them read-only against the account. existing is the current tfvars
// (nil on a fresh install), for defaults.
func (c *InitCmd) chooseWeb(env *initEnv, s *settings, cloud initCloud, existing map[string]string, record func(checkResult)) error {
	w := env.out
	first := func(k string) string {
		if v := tfStrings(existing[k]); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	// What the install already has, carried forward unless a flag changes it.
	prevDomain, prevHost := first("web_domain"), first("web_hostname")
	if prevHost == "" && prevDomain != "" {
		prevHost = "darkplum." + prevDomain
	}

	// Shown on every path, flags included: the name is public.
	fmt.Fprintf(w, "  %s\n", sDim("Your team reaches Darkplum at a private HTTPS address: darkplum.<domain>, from your VPC and"))
	fmt.Fprintf(w, "  %s\n", sDim("the networks you allow. The name is public (certificate transparency logs), its address is not."))
	domain := normalizeDNSName(c.Domain)
	if domain == "" {
		switch {
		case env.interactive:
			var err error
			domain, err = env.prompt.Ask("Domain for Darkplum's address (a domain you control, e.g. acme.com)", prevDomain, func(v string) error {
				v = normalizeDNSName(v)
				if v == "" {
					return errors.New("enter a domain you control, such as acme.com")
				}
				return validateTFWebDomain(v)
			})
			if err != nil {
				return err
			}
			domain = normalizeDNSName(domain)
		case prevDomain != "":
			domain = prevDomain
		default:
			return env.fail("domain", "not given", "pass --domain <a domain you control>, e.g. --domain acme.com: the team reaches Darkplum at darkplum.<domain>", "--domain is required")
		}
	}
	if err := validateTFWebDomain(domain); err != nil || domain == "" {
		return env.fail("domain", domain, "use a lowercase DNS name you control, such as acme.com", "invalid --domain")
	}
	host := normalizeDNSName(c.Hostname)
	switch {
	case host != "":
	case domain == prevDomain:
		host = prevHost
	default:
		host = "darkplum." + domain
	}
	if err := validateTFWebHostname(domain, host); err != nil {
		return env.fail("hostname", host, "--hostname "+err.Error(), "invalid --hostname")
	}
	// The install already owns this name: its zone and records exist (the
	// alias record is its own), so nothing is looked up or claimed again, and
	// a zone Terraform created stays Terraform's.
	owned := prevDomain != "" && domain == prevDomain && host == prevHost && c.ZoneID == ""
	ws := webSettings{domain: domain, hostname: host, zoneID: c.ZoneID, certARN: c.CertificateARN}
	if owned {
		ws.zoneID = first("web_zone_id")
		if ws.certARN == "" {
			ws.certARN = first("web_certificate_arn")
		}
	}
	if err := validateTFWebZoneID(domain, ws.zoneID); err != nil {
		return env.fail("hosted zone", ws.zoneID, "--zone-id "+err.Error(), "invalid --zone-id")
	}
	if err := validateTFWebCertARN(domain, s.region, ws.certARN); err != nil {
		return env.fail("certificate", ws.certARN, "--certificate-arn "+err.Error(), "invalid --certificate-arn")
	}

	fmt.Fprintf(w, "%s checking DNS for %s…\n", sDim("…"), host)
	if owned {
		detail := host + " (unchanged; zone managed by Terraform)"
		if ws.zoneID != "" {
			detail = host + " (unchanged; zone " + ws.zoneID + ")"
		}
		record(checkResult{name: "hosted zone", status: checkPass, detail: detail})
	} else if ws.zoneID != "" {
		z, err := cloud.HostedZone(ws.zoneID)
		if err != nil {
			return env.fail("hosted zone", ws.zoneID+": "+firstLine(err.Error()), "check the zone ID (route53:GetHostedZone)", "cannot read --zone-id")
		}
		if z.Private || !(host == z.Name || strings.HasSuffix(host, "."+z.Name)) {
			return env.fail("hosted zone", fmt.Sprintf("%s is %s, which does not hold %s publicly", ws.zoneID, z.Name, host),
				"pass a public zone whose name is "+host+" or a parent of it", "--zone-id does not cover the hostname")
		}
		if err := checkFreeName(env, cloud, ws.zoneID, host, host == z.Name); err != nil {
			return err
		}
		record(checkResult{name: "hosted zone", status: checkPass, detail: z.Name + " (yours; records are added, the zone is never changed otherwise)"})
	} else if z, ok, err := cloud.PublicZone(host); err != nil {
		return env.fail("hosted zone", firstLine(err.Error()), "needs route53:ListHostedZonesByName", "cannot read Route53")
	} else if ok {
		// A zone for the hostname already exists here (an earlier init made it,
		// or you did): use it as is.
		if err := checkFreeName(env, cloud, z.ID, host, true); err != nil {
			return err
		}
		if _, parentHere, perr := cloud.PublicZone(domain); perr == nil && !parentHere {
			// Its delegation lives at another DNS provider: an earlier init
			// stopped while waiting for it. Resume that wait before writing.
			ws.createZone = true
			record(checkResult{name: "hosted zone", status: checkWarn, detail: host + " (created by an earlier init)",
				hint: "its NS records must be added at " + domain + "'s DNS provider; init checks before saving"})
		} else {
			ws.zoneID = z.ID
			record(checkResult{name: "hosted zone", status: checkPass, detail: host + " (already in this account; reused)"})
		}
	} else if parent, ok, err := cloud.PublicZone(domain); err != nil {
		return env.fail("hosted zone", firstLine(err.Error()), "needs route53:ListHostedZonesByName", "cannot read Route53")
	} else if ok {
		if err := checkFreeName(env, cloud, parent.ID, host, false); err != nil {
			return err
		}
		record(checkResult{name: "hosted zone", status: checkPass, detail: "creates " + host + ", delegated from " + domain + " in this account"})
	} else {
		ws.createZone = true
		record(checkResult{name: "hosted zone", status: checkWarn, detail: domain + "'s DNS is not in this account",
			hint: "init creates a zone for " + host + " and shows 4 name-server records to add at your DNS provider"})
	}

	if ws.certARN != "" {
		ci, err := cloud.Certificate(ws.certARN)
		if err != nil {
			return env.fail("certificate", firstLine(err.Error()), "check the ARN (acm:DescribeCertificate in "+s.region+")", "cannot read --certificate-arn")
		}
		if err := certUsable(ci, ws.certARN, s.account, host, env.now()); err != nil {
			return env.fail("certificate", err.Error(), "pass a certificate for "+host+" in this account and region, or leave --certificate-arn off to have one issued", "--certificate-arn is not usable")
		}
		record(checkResult{name: "certificate", status: checkPass, detail: "yours, valid until " + ci.NotAfter.Format("2006-01-02")})
	} else {
		record(checkResult{name: "certificate", status: checkPass, detail: "issued for " + host + " by ACM (DNS-validated, renews itself)"})
	}

	vpc := first("vpc_cidr")
	if vpc == "" {
		vpc = defaultVPCCIDR
	}
	allowed := splitCIDRs(c.AllowedCIDRs)
	if len(allowed) == 0 {
		if prev := tfStrings(existing["web_allowed_cidrs"]); len(prev) > 0 {
			allowed = prev
		}
	}
	if c.AllowedCIDRs == "" && env.interactive {
		def := vpc
		if len(allowed) > 0 {
			def = strings.Join(allowed, ", ")
		}
		ans, err := env.prompt.Ask("Networks allowed to reach it (your VPC; add VPN or office ranges, comma-separated)", def, validateCIDRList)
		if err != nil {
			return err
		}
		allowed = splitCIDRs(ans)
	}
	if len(allowed) == 0 {
		allowed = []string{vpc}
	}
	if err := validateTFWebCIDRs(domain, allowed); err != nil {
		return env.fail("allowed networks", strings.Join(allowed, ", "), "--allowed-cidrs: "+err.Error(), "invalid --allowed-cidrs")
	}
	ws.allowed = allowed
	record(checkResult{name: "web address", status: checkPass, detail: "https://" + host + " from " + strings.Join(allowed, ", ")})
	s.web = ws
	return nil
}

// checkFreeName refuses a hostname that already has records in zoneID: the
// install never overwrites a name that points elsewhere. At a zone's own apex
// its SOA and NS records are expected.
func checkFreeName(env *initEnv, cloud initCloud, zoneID, host string, apex bool) error {
	types, err := cloud.RecordTypes(zoneID, host)
	if err != nil {
		return env.fail("hostname", firstLine(err.Error()), "needs route53:ListResourceRecordSets", "cannot read the zone's records")
	}
	var taken []string
	for _, t := range types {
		if !(apex && (t == "SOA" || t == "NS")) {
			taken = append(taken, t)
		}
	}
	if len(taken) > 0 {
		sort.Strings(taken)
		return env.fail("hostname", host+" already has "+strings.Join(taken, ", ")+" records",
			"pick another name with --hostname, or remove those records first", "hostname already in use")
	}
	return nil
}

// certUsable checks a supplied certificate: issued, more than 30 days left,
// this account and region, and covering host.
func certUsable(ci certInfo, arn, account, host string, now time.Time) error {
	m := certARNRe.FindStringSubmatch(arn)
	switch {
	case m == nil:
		return errors.New("not an ACM certificate ARN")
	case m[2] != account:
		return fmt.Errorf("it belongs to account %s, the install is in %s", m[2], account)
	case ci.Status != "ISSUED":
		return fmt.Errorf("its status is %s, not ISSUED", ci.Status)
	case ci.NotAfter.Before(now.Add(30 * 24 * time.Hour)):
		return fmt.Errorf("it expires on %s (less than 30 days)", ci.NotAfter.Format("2006-01-02"))
	case !certCovers(ci.Names, host):
		return fmt.Errorf("it covers %s, not %s", strings.Join(ci.Names, ", "), host)
	}
	return nil
}

// createAndDelegate creates the zone for the hostname (when the domain's DNS
// is elsewhere), shows the records to add, and waits, visibly and bounded,
// until public DNS answers with them. Re-running init resumes: the zone is
// found again and the wait starts over.
func (c *InitCmd) createAndDelegate(env *initEnv, s *settings, cloud initCloud) error {
	w := env.out
	fmt.Fprintf(w, "%s creating hosted zone %s…\n", sDim("…"), s.web.hostname)
	z, err := cloud.CreateWebZone(s.web.hostname)
	if err != nil {
		return env.fail("hosted zone", firstLine(err.Error()), "needs route53:CreateHostedZone; re-run init (it reuses what was created)", "could not create the hosted zone")
	}
	s.web.zoneID, s.web.nameServers = z.ID, z.NameServers
	fmt.Fprintf(w, "\n  Add these records at the DNS provider for %s:\n\n", sInfo(s.web.domain))
	for _, ns := range z.NameServers {
		fmt.Fprintf(w, "    %s  NS  %s\n", s.web.hostname, ns)
	}
	fmt.Fprintf(w, "\n  %s\n", sDim("Waiting until public DNS shows them (Ctrl-C stops; re-run init to continue)."))
	timeout := env.delegationTimeout
	start := env.now()
	for i := 0; ; i++ {
		got, _ := env.lookupNS(s.web.hostname)
		if delegated(got, z.NameServers) {
			printCheckTo(w, checkResult{name: "delegation", status: checkPass, detail: s.web.hostname + " is delegated to its zone"})
			return nil
		}
		elapsed := env.now().Sub(start)
		if elapsed >= timeout {
			return env.fail("delegation", fmt.Sprintf("not visible after %s", minutes(timeout)),
				"add the 4 NS records above at your DNS provider, then re-run ./bin/darkplum init: it continues from here. Nothing else was written", "delegation not visible")
		}
		if i%4 == 0 {
			fmt.Fprintf(w, "  %s waiting for the NS records %s\n", sDim("…"), sDim(fmt.Sprintf("(%d min)", int(elapsed.Minutes()))))
		}
		env.sleep(15 * time.Second)
	}
}

// existingTFVars is the parsed tfvars at path, or nil when there is none.
func existingTFVars(path string) map[string]string {
	tf, err := readTFVars(path)
	if err != nil {
		return nil
	}
	return tf.Raw
}

// mergeWeb adds the private web address to an existing install's settings:
// it asks only the web questions and appends the web_* lines, leaving every
// other line as it was (a .bak copy is kept).
func (c *InitCmd) mergeWeb(env *initEnv, dir string) error {
	w := env.out
	path := filepath.Join(dir, "terraform.tfvars")
	tf, err := readTFVars(path)
	if err != nil {
		return env.fail("install settings", firstLine(err.Error()), "re-run ./bin/darkplum init and choose Start over", "cannot read the install settings")
	}
	s := &settings{profile: tf.Profile, region: tf.Region}
	record := func(r checkResult) {
		printCheckTo(w, r)
		s.checks = append(s.checks, r)
	}
	if s.region == "" {
		return env.fail("install settings", "no region", "re-run ./bin/darkplum init and choose Start over", "install settings name no region")
	}
	cloud := env.newCloud(tf.Profile)
	fmt.Fprintf(w, "%s resolving AWS identity…\n", sDim("…"))
	id, err := cloud.Identity()
	if err != nil {
		return env.fail("aws credentials", firstLine(err.Error()), "run `aws sso login` / `aws configure` for the install's profile", "cannot resolve AWS credentials")
	}
	pinned := false
	for _, a := range tf.AccountIDs {
		pinned = pinned || a == id.Account
	}
	if !pinned {
		return env.fail("aws account", fmt.Sprintf("credentials are for %s, the install is in %s", id.Account, strings.Join(tf.AccountIDs, ",")),
			"use the install's profile", "credentials for another account")
	}
	s.account = id.Account
	record(checkResult{name: "aws account", status: checkPass, detail: id.Account + " (from the install settings)"})
	if err := c.chooseWeb(env, s, cloud, tf.Raw, record); err != nil {
		return err
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// The address's own lines are replaced (a change), every other line is kept.
	text := rewriteTFVars(string(b), func(k string) bool { return webKeys[k] }, s.web.kvs())
	fmt.Fprintln(w)
	if c.DryRun {
		fmt.Fprintf(w, "%s\n%s\n", sHeader("terraform.tfvars (dry run — not written)"), text)
		if s.web.createZone {
			fmt.Fprintf(w, "  %s would create the public hosted zone %s and wait for its delegation\n\n", sWarn("!"), s.web.hostname)
		}
		fmt.Fprintln(w, sSuccess("✓ dry run complete — nothing was written or created"))
		return nil
	}
	if env.interactive {
		what := "Add the private address to this install's settings?"
		if s.web.createZone {
			what = fmt.Sprintf("Add the private address? This creates a public hosted zone for %s", s.web.hostname)
		}
		ok, err := env.prompt.Confirm(what, true)
		if err != nil {
			return err
		}
		if !ok {
			return errAborted
		}
	}
	if s.web.createZone {
		if err := c.createAndDelegate(env, s, cloud); err != nil {
			return err
		}
		text += fmt.Sprintf("web_zone_id = %s\n", hclString(s.web.zoneID))
	}
	if err := writeFileAtomic(path, []byte(text)); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%s\n", sSuccess("✓ private address added; every other setting is unchanged"))
	fmt.Fprintf(w, "  Your team's address after deploy: %s %s\n", sCmd("https://"+s.web.hostname), sDim("(from "+strings.Join(s.web.allowed, ", ")+")"))
	fmt.Fprintf(w, "\n  Next: %s, then %s\n", sCmd("make deploy"), sCmd("./bin/darkplum configure"))
	return nil
}
