package db

import (
	"fmt"
	"strings"
)

// UpsertHTTPServiceReq is the input for UpsertHTTPService.
type UpsertHTTPServiceReq struct {
	URL           string
	Technologies  []string
	StatusCode    *int
	ContentLength *int64
	PageTitle     string
	FaviconMMH3   string
	Auth          []map[string]any
	IP            string // optional, from async DNS
	TaskID        int64
}

// UpsertHTTPService inserts or merges an HTTP service asset. Domain, port,
// service_name, and root_domain are auto-extracted from URL.
func (s *AssetStore) UpsertHTTPService(req UpsertHTTPServiceReq) (int64, error) {
	if req.URL == "" {
		return 0, fmt.Errorf("url is required")
	}
	if err := ValidateAssetIP(req.IP); err != nil {
		return 0, err
	}
	if s.tx == nil {
		return s.withCompanyScopeMutation(func(scoped *AssetStore) (int64, error) {
			return scoped.UpsertHTTPService(req)
		})
	}
	normURL := normalizeURL(req.URL)
	domain, port, serviceName := parseURL(normURL)
	rootDomain, _ := RootDomain(domain)
	if rootDomain == "" {
		rootDomain = domain
	}

	cseg := calcCSegment(req.IP)
	companyID, err := s.resolveCompanyWithICP(rootDomain, req.IP, "")
	if err != nil {
		return 0, err
	}

	var taskIDs string
	if req.TaskID > 0 {
		taskIDs = fmt.Sprintf("{%d}", req.TaskID)
	} else {
		taskIDs = "{}"
	}

	techsArr := marshalStringArray(req.Technologies)
	authJSON, err := marshalJSONBArray(req.Auth)
	if err != nil {
		return 0, err
	}

	var domainVal, ipVal, csegVal, titleVal, faviconVal any
	if domain != "" {
		domainVal = domain
	}
	if req.IP != "" {
		ipVal = req.IP
	}
	if cseg != "" {
		csegVal = cseg
	}
	if req.PageTitle != "" {
		titleVal = req.PageTitle
	}
	if req.FaviconMMH3 != "" {
		faviconVal = req.FaviconMMH3
	}

	var id int64
	err = s.tx.QueryRow(`
INSERT INTO assets(
    type, url, service_type, service_name, domain, ip, port, root_domain,
    c_segment, favicon_mmh3, technologies, status_code, content_length, page_title,
    auth, company_id, company_source, task_ids
)
VALUES (
    'service', $1, 'http', $2, $3, $4, $5, $6,
    $7::cidr, $8, $9::text[], $10, $11, $12,
    $13::jsonb[], $14, 'scope', $15::bigint[]
)
ON CONFLICT (url) WHERE type = 'service' AND service_type = 'http' DO UPDATE SET
    status_code    = COALESCE(EXCLUDED.status_code,    assets.status_code),
    content_length = COALESCE(EXCLUDED.content_length, assets.content_length),
    page_title     = COALESCE(EXCLUDED.page_title,     assets.page_title),
    favicon_mmh3   = COALESCE(EXCLUDED.favicon_mmh3,   assets.favicon_mmh3),
    ip             = COALESCE(EXCLUDED.ip,             assets.ip),
    c_segment      = COALESCE(EXCLUDED.c_segment,      assets.c_segment),
    technologies   = (SELECT ARRAY(SELECT DISTINCT unnest(assets.technologies || EXCLUDED.technologies))),
    auth           = (
        SELECT ARRAY(
            SELECT DISTINCT ON (elem::text) elem
            FROM unnest(assets.auth || EXCLUDED.auth) AS elem
        )
    ),
    task_ids       = (SELECT ARRAY(SELECT DISTINCT unnest(assets.task_ids || EXCLUDED.task_ids))),
    company_id     = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN assets.company_id
        ELSE COALESCE(EXCLUDED.company_id, assets.company_id)
    END,
    company_source = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN 'explicit'
        WHEN EXCLUDED.company_id IS NOT NULL THEN 'scope'
        ELSE assets.company_source
    END,
    last_seen      = now()
RETURNING id`,
		normURL, serviceName, domainVal, ipVal, nullableInt(port), rootDomain,
		csegVal, faviconVal, techsArr, req.StatusCode, req.ContentLength, titleVal,
		authJSON, companyID, taskIDs,
	).Scan(&id)
	if err != nil {
		return 0, err
	}

	// side effects: register root_domain + subdomain as their own assets too
	s.linkHostAssets(domain, rootDomain, req.TaskID)
	if req.IP != "" {
		var boundDomains []string
		if domain != "" {
			boundDomains = []string{domain}
		}
		var openPorts []PortService
		if port > 0 {
			openPorts = []PortService{{Port: port, Service: serviceName}}
		}
		_, _ = s.UpsertIP(UpsertIPReq{
			IP:           req.IP,
			BoundDomains: boundDomains,
			OpenPorts:    openPorts,
			TaskID:       req.TaskID,
		})
	}
	return id, nil
}

// =====================================================================
// UpsertOtherService
// =====================================================================

// UpsertOtherServiceReq is the input for UpsertOtherService.
type UpsertOtherServiceReq struct {
	Domain      string // domain or ip required
	IP          string
	Port        int
	ServiceName string
	Auth        []map[string]any
	TaskID      int64
}

// UpsertOtherService inserts or merges a non-HTTP service asset.
func (s *AssetStore) UpsertOtherService(req UpsertOtherServiceReq) (int64, error) {
	if req.Domain == "" && req.IP == "" {
		return 0, fmt.Errorf("domain or ip is required")
	}
	if err := ValidateAssetIP(req.IP); err != nil {
		return 0, err
	}
	if req.Port == 0 {
		return 0, fmt.Errorf("port is required")
	}
	if req.ServiceName == "" {
		return 0, fmt.Errorf("service_name is required")
	}
	if s.tx == nil {
		return s.withCompanyScopeMutation(func(scoped *AssetStore) (int64, error) {
			return scoped.UpsertOtherService(req)
		})
	}
	// normalize service_name (lowercase) so the (domain,ip,port,service_name)
	// dedup key doesn't split "SSH" and "ssh" into separate rows.
	serviceName := strings.ToLower(strings.TrimSpace(req.ServiceName))

	domain := DomainKey(req.Domain)
	var rootDomain string
	if domain != "" {
		rootDomain, _ = RootDomain(domain)
		if rootDomain == "" {
			rootDomain = domain
		}
	}

	cseg := calcCSegment(req.IP)
	companyID, err := s.resolveCompanyWithICP(rootDomain, req.IP, "")
	if err != nil {
		return 0, err
	}

	var taskIDs string
	if req.TaskID > 0 {
		taskIDs = fmt.Sprintf("{%d}", req.TaskID)
	} else {
		taskIDs = "{}"
	}

	authJSON, err := marshalJSONBArray(req.Auth)
	if err != nil {
		return 0, err
	}

	var domainVal, rootDomainVal, ipVal, csegVal any
	if domain != "" {
		domainVal = domain
	}
	if rootDomain != "" {
		rootDomainVal = rootDomain
	}
	if req.IP != "" {
		ipVal = req.IP
	}
	if cseg != "" {
		csegVal = cseg
	}

	var id int64
	err = s.tx.QueryRow(`
INSERT INTO assets(
    type, service_type, service_name, domain, ip, port, root_domain,
    c_segment, auth, company_id, company_source, task_ids
)
VALUES ('service', 'other', $1, $2, $3, $4, $5, $6::cidr, $7::jsonb[], $8, 'scope', $9::bigint[])
ON CONFLICT (COALESCE(domain,''), COALESCE(ip,''), port, service_name) WHERE type = 'service' AND service_type = 'other' DO UPDATE SET
    domain     = COALESCE(EXCLUDED.domain,     assets.domain),
    ip         = COALESCE(EXCLUDED.ip,         assets.ip),
    c_segment  = COALESCE(EXCLUDED.c_segment,  assets.c_segment),
    auth       = (
        SELECT ARRAY(
            SELECT DISTINCT ON (elem::text) elem
            FROM unnest(assets.auth || EXCLUDED.auth) AS elem
        )
    ),
    task_ids   = (SELECT ARRAY(SELECT DISTINCT unnest(assets.task_ids || EXCLUDED.task_ids))),
    company_id = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN assets.company_id
        ELSE COALESCE(EXCLUDED.company_id, assets.company_id)
    END,
    company_source = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN 'explicit'
        WHEN EXCLUDED.company_id IS NOT NULL THEN 'scope'
        ELSE assets.company_source
    END,
    last_seen  = now()
RETURNING id`,
		serviceName, domainVal, ipVal, req.Port, rootDomainVal,
		csegVal, authJSON, companyID, taskIDs,
	).Scan(&id)
	if err != nil {
		return 0, err
	}

	// side effects
	if req.IP != "" && req.Port > 0 {
		var boundDomains []string
		if domain != "" {
			boundDomains = []string{domain}
		}
		_, _ = s.UpsertIP(UpsertIPReq{
			IP:           req.IP,
			BoundDomains: boundDomains,
			OpenPorts:    []PortService{{Port: req.Port, Service: serviceName}},
			TaskID:       req.TaskID,
		})
	}
	s.linkHostAssets(domain, rootDomain, req.TaskID)
	return id, nil
}

// UpsertEndpointReq is the input for UpsertEndpoint.
type UpsertEndpointReq struct {
	URL    string
	Method string
	Params []map[string]any
	IP     string // optional
	TaskID int64
}

// UpsertEndpoint inserts or merges an endpoint asset. Domain, port, root_domain
// are auto-extracted from the URL.
func (s *AssetStore) UpsertEndpoint(req UpsertEndpointReq) (int64, error) {
	if req.URL == "" {
		return 0, fmt.Errorf("url is required")
	}
	if req.Method == "" {
		return 0, fmt.Errorf("method is required")
	}
	if err := ValidateAssetIP(req.IP); err != nil {
		return 0, err
	}
	if s.tx == nil {
		return s.withCompanyScopeMutation(func(scoped *AssetStore) (int64, error) {
			return scoped.UpsertEndpoint(req)
		})
	}
	method := strings.ToUpper(req.Method)
	normURL := normalizeURL(req.URL)
	domain, port, _ := parseURL(normURL)
	rootDomain, _ := RootDomain(domain)
	if rootDomain == "" {
		rootDomain = domain
	}

	cseg := calcCSegment(req.IP)
	companyID, err := s.resolveCompanyWithICP(rootDomain, req.IP, "")
	if err != nil {
		return 0, err
	}

	var taskIDs string
	if req.TaskID > 0 {
		taskIDs = fmt.Sprintf("{%d}", req.TaskID)
	} else {
		taskIDs = "{}"
	}

	paramsJSON, err := marshalJSONBArray(req.Params)
	if err != nil {
		return 0, err
	}

	var domainVal, rootDomainVal, ipVal, csegVal any
	if domain != "" {
		domainVal = domain
	}
	if rootDomain != "" {
		rootDomainVal = rootDomain
	}
	if req.IP != "" {
		ipVal = req.IP
	}
	if cseg != "" {
		csegVal = cseg
	}

	var id int64
	err = s.tx.QueryRow(`
INSERT INTO assets(
    type, url, method, domain, ip, port, root_domain,
    params, company_id, company_source, task_ids, c_segment
)
VALUES ('endpoint', $1, $2, $3, $4, $5, $6, $7::jsonb[], $8, 'scope', $9::bigint[], $10::cidr)
ON CONFLICT (url, method) WHERE type = 'endpoint' DO UPDATE SET
    params     = (
        SELECT ARRAY(
            SELECT DISTINCT ON ((elem->>'location'), (elem->>'name')) elem
            FROM unnest(assets.params || EXCLUDED.params) AS elem
            ORDER BY (elem->>'location'), (elem->>'name'), elem::text DESC
        )
    ),
    ip         = COALESCE(EXCLUDED.ip,        assets.ip),
    c_segment  = COALESCE(EXCLUDED.c_segment, assets.c_segment),
    task_ids   = (SELECT ARRAY(SELECT DISTINCT unnest(assets.task_ids || EXCLUDED.task_ids))),
    company_id = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN assets.company_id
        ELSE COALESCE(EXCLUDED.company_id, assets.company_id)
    END,
    company_source = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN 'explicit'
        WHEN EXCLUDED.company_id IS NOT NULL THEN 'scope'
        ELSE assets.company_source
    END,
    last_seen  = now()
RETURNING id`,
		normURL, method, domainVal, ipVal, nullableInt(port), rootDomainVal,
		paramsJSON, companyID, taskIDs, csegVal,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	// side effects: endpoint previously registered none — register its host as
	// root_domain + subdomain(+IP) so those asset types get populated too.
	s.linkHostAssets(domain, rootDomain, req.TaskID)
	if req.IP != "" {
		_, _ = s.UpsertIP(UpsertIPReq{IP: req.IP, TaskID: req.TaskID})
	}
	return id, nil
}

// =====================================================================
// Query helpers
// =====================================================================
