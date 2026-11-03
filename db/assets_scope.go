package db

import (
	"encoding/json"
	"net"
	"net/url"
	"strings"
)

// withCompanyScopeMutation serializes scope resolution and every asset write
// that consumes its result in one transaction. Nested asset side effects reuse
// the same transaction through the scoped store.
func (s *AssetStore) withCompanyScopeMutation(fn func(*AssetStore) (int64, error)) (int64, error) {
	if s.tx != nil {
		return fn(s)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, err
	}
	scoped := &AssetStore{db: s.db, company: s.company, tx: tx}
	id, err := fn(scoped)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *AssetStore) resolveCompanyWithICP(rootDomain, ipStr, icp string) (*int64, error) {
	if s.tx != nil {
		return resolveCompanyWithICP(s.tx, rootDomain, ipStr, icp)
	}
	return s.company.ResolveCompanyWithICP(rootDomain, ipStr, icp)
}

// =====================================================================
// Helpers
// =====================================================================

// calcCSegment computes the /24 (IPv4) or /48 (IPv6) network for an IP string.
func calcCSegment(ipStr string) string {
	if ipStr == "" {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		// IPv4 /24
		parts := strings.Split(ipStr, ".")
		if len(parts) == 4 {
			return parts[0] + "." + parts[1] + "." + parts[2] + ".0/24"
		}
		return ""
	}
	// IPv6 /48
	_, ipnet, err := net.ParseCIDR(ipStr + "/48")
	if err != nil {
		return ""
	}
	return ipnet.String()
}

// normalizeURL lowercases scheme and host, strips trailing slash from bare roots.
func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	s := u.String()
	if strings.HasSuffix(s, "/") && u.Path == "/" && u.RawQuery == "" {
		s = strings.TrimSuffix(s, "/")
	}
	return s
}

// parseURL extracts domain, port, service_name from a URL.
func parseURL(raw string) (domain string, port int, serviceName string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, ""
	}
	domain = strings.ToLower(u.Hostname())
	port = defaultPort(strings.ToLower(u.Scheme), u.Port())
	switch strings.ToLower(u.Scheme) {
	case "https":
		serviceName = "HTTPS"
	case "http":
		serviceName = "HTTP"
	default:
		serviceName = strings.ToUpper(u.Scheme)
	}
	return
}

func marshalJSONBArray(items []map[string]any) (string, error) {
	if len(items) == 0 {
		return "{}", nil
	}
	parts := make([]string, len(items))
	for i, m := range items {
		b, err := json.Marshal(m)
		if err != nil {
			return "", err
		}
		// PostgreSQL array literal for jsonb[]: each element must be
		// double-quoted with internal backslashes and double-quotes escaped.
		s := strings.ReplaceAll(string(b), `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		parts[i] = `"` + s + `"`
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

func marshalStringArray(items []string) string {
	if len(items) == 0 {
		return "{}"
	}
	escaped := make([]string, len(items))
	for i, s := range items {
		escaped[i] = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "{" + strings.Join(escaped, ",") + "}"
}

func marshalPortServices(ps []PortService) (string, error) {
	items := make([]map[string]any, len(ps))
	for i, p := range ps {
		items[i] = map[string]any{"port": p.Port, "service": p.Service}
	}
	return marshalJSONBArray(items)
}

// nullableInt64 returns sql.NullInt64.
func nullableInt(v int) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// =====================================================================
// UpsertRootDomain
// =====================================================================
