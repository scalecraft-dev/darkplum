package provider

import (
	"fmt"
	"net/url"
)

// ValidateVendorBaseURL checks that rawURL is a usable override for a direct
// vendor API provider's base URL (ANTHROPIC_BASE_URL, OPENAI_BASE_URL). An
// empty rawURL is always valid — it means "use the vendor SDK's own
// production default" and neither provider treats it as configuration to
// validate.
//
// Outside DEV_MODE, the scheme must be https: a vendor API key travels in
// this request, and a plaintext http override — whether from a typo or a
// compromised env — would send it in the clear. DEV_MODE additionally allows
// http, since tests and local development point this at an httptest server
// or a local proxy.
//
// name is the env var being validated ("ANTHROPIC_BASE_URL",
// "OPENAI_BASE_URL"), named in the returned error only, so every caller of
// this shared helper gets an error pointing at the knob it actually needs to
// fix.
func ValidateVendorBaseURL(name, rawURL string, devMode bool) error {
	if rawURL == "" {
		return nil
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%s: invalid URL %q: %w", name, rawURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s: URL %q must include scheme and host", name, rawURL)
	}

	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && devMode {
		return nil
	}

	if devMode {
		return fmt.Errorf("%s: URL %q must use https or http, got %q", name, rawURL, u.Scheme)
	}
	return fmt.Errorf(
		"%s: URL %q must use https (a vendor API key is sent with every request); "+
			"set DEV_MODE=true to allow http in development",
		name, rawURL,
	)
}
