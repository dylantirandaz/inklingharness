package permission

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// hostPattern matches the host of a web_fetch URL. It holds only lowercase
// letters, digits, '.', '-', '_' and the wildcard '*'. A port, a scheme or a
// path in the pattern is an error, because such a pattern could never match
// and the user would not see why.
type hostPattern string

func parseHostPattern(pattern string) (hostPattern, error) {
	lower := strings.ToLower(pattern)
	for i := range len(lower) {
		character := lower[i]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') &&
			character != '.' && character != '-' && character != '_' && character != '*' {
			return "", errors.New("pattern must be a host without scheme, port or path, for example github.com or *.go.dev")
		}
	}
	return hostPattern(lower), nil
}

// decodeHost returns the lowercase host of the URL in a web_fetch input.
// The fetch tool reads only http and https URLs, so other URLs are not
// valid. One trailing dot is removed, because "github.com." names the same
// host as "github.com" and must not get past a deny rule.
func decodeHost(input json.RawMessage) (string, bool) {
	var arguments struct {
		URL *string `json:"url"`
	}
	if err := json.Unmarshal(input, &arguments); err != nil || arguments.URL == nil {
		return "", false
	}
	parsed, err := url.Parse(*arguments.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return "", false
	}
	return host, true
}

func (pattern hostPattern) denies(host string) bool {
	return matchWildcard(string(pattern), host)
}

func (pattern hostPattern) allows(host string) bool {
	return matchWildcard(string(pattern), host)
}
