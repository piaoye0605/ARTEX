package db

import (
	"database/sql"
	"encoding/json"
)

// =====================================================================
// 统一资产表
// =====================================================================

// Asset is a row in the assets table.
type Asset struct {
	ID         int64   `json:"id"`
	Type       string  `json:"type"`
	CompanyID  *int64  `json:"company_id,omitempty"`
	TaskIDs    []int64 `json:"task_ids"`
	Domain     string  `json:"domain,omitempty"`
	RootDomain string  `json:"root_domain,omitempty"`
	IP         string  `json:"ip,omitempty"`
	CSegment   string  `json:"c_segment,omitempty"`
	Port       *int    `json:"port,omitempty"`
	ICP        string  `json:"icp,omitempty"`
	// ip fields
	BoundDomains []string         `json:"bound_domains,omitempty"`
	OpenPorts    []map[string]any `json:"open_ports,omitempty"`
	// subdomain fields
	RecordType  string   `json:"record_type,omitempty"`
	RecordValue []string `json:"record_value,omitempty"`
	// app fields
	BundleID       string `json:"bundle_id,omitempty"`
	AppName        string `json:"app_name,omitempty"`
	Category       string `json:"category,omitempty"`
	AppDescription string `json:"app_description,omitempty"`
	AppICP         string `json:"app_icp,omitempty"`
	// service fields
	URL           string           `json:"url,omitempty"`
	ServiceType   string           `json:"service_type,omitempty"`
	ServiceName   string           `json:"service_name,omitempty"`
	FaviconMMH3   string           `json:"favicon_mmh3,omitempty"`
	StatusCode    *int             `json:"status_code,omitempty"`
	ContentLength *int64           `json:"content_length,omitempty"`
	PageTitle     string           `json:"page_title,omitempty"`
	Technologies  []string         `json:"technologies,omitempty"`
	Auth          []map[string]any `json:"auth,omitempty"`
	// endpoint fields
	Method string           `json:"method,omitempty"`
	Params []map[string]any `json:"params,omitempty"`
	// meta
	Extra             map[string]any `json:"extra,omitempty"`
	LastSeen          string         `json:"last_seen"`
	TaskSource        string         `json:"task_source,omitempty"`
	TaskSourceSummary string         `json:"task_source_summary,omitempty"`
	TaskSourceNodeID  *int64         `json:"task_source_node_id,omitempty"`
}

// AuthItem is one entry in the auth array.
type AuthItem struct {
	Type        string `json:"type,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	Token       string `json:"token,omitempty"`
	Description string `json:"description,omitempty"`
}

// ParamItem is one entry in the params array.
type ParamItem struct {
	Location string `json:"location"`
	Name     string `json:"name"`
	Value    string `json:"value,omitempty"`
	Type     string `json:"type,omitempty"`
}

// PortService is one entry in open_ports: {"port":22,"service":"ssh"}.
type PortService struct {
	Port    int    `json:"port"`
	Service string `json:"service,omitempty"`
}

// AssetStore operates on the assets table.
type AssetStore struct {
	db      *DB
	company *CompanyStore
	tx      *sql.Tx
}

// Assets returns the asset store.
func (d *DB) Assets() *AssetStore {
	return &AssetStore{db: d, company: d.Companies()}
}

// Companies returns the company store associated with this asset store.
func (s *AssetStore) Companies() *CompanyStore { return s.company }

const assetSelectCols = `SELECT id, type, company_id, array_to_json(task_ids)::text,
       COALESCE(domain,''), COALESCE(root_domain,''), COALESCE(ip,''),
       COALESCE(c_segment::text,''), port,
       COALESCE(icp,''), array_to_json(bound_domains)::text, array_to_json(open_ports)::text, COALESCE(record_type,''),
       array_to_json(record_value)::text, COALESCE(bundle_id,''), COALESCE(app_name,''),
       COALESCE(category,''), COALESCE(app_description,''), COALESCE(app_icp,''),
       COALESCE(url,''), COALESCE(service_type,''), COALESCE(service_name,''),
       COALESCE(favicon_mmh3,''), status_code, content_length,
       COALESCE(page_title,''), array_to_json(technologies)::text, array_to_json(auth)::text,
       COALESCE(method,''), array_to_json(params)::text, extra, last_seen::text
FROM assets`

func scanTypeCounts(rows *sql.Rows) (map[string]int, error) {
	out := map[string]int{}
	for rows.Next() {
		var typ string
		var cnt int
		if err := rows.Scan(&typ, &cnt); err != nil {
			return nil, err
		}
		out[typ] = cnt
	}
	return out, rows.Err()
}

// scanAssets scans the wide SELECT that covers all type columns.
func scanAssets(rows *sql.Rows) ([]*Asset, error) {
	var out []*Asset
	for rows.Next() {
		a := &Asset{}
		var taskIDsRaw, boundDomainsRaw, openPortsRaw, recordValueRaw, techsRaw, authRaw, paramsRaw, extraRaw []byte
		var port, statusCode sql.NullInt64
		var contentLength sql.NullInt64
		var companyID sql.NullInt64
		if err := rows.Scan(
			&a.ID, &a.Type, &companyID, &taskIDsRaw,
			&a.Domain, &a.RootDomain, &a.IP, &a.CSegment, &port,
			&a.ICP, &boundDomainsRaw, &openPortsRaw, &a.RecordType,
			&recordValueRaw, &a.BundleID, &a.AppName,
			&a.Category, &a.AppDescription, &a.AppICP,
			&a.URL, &a.ServiceType, &a.ServiceName,
			&a.FaviconMMH3, &statusCode, &contentLength,
			&a.PageTitle, &techsRaw, &authRaw,
			&a.Method, &paramsRaw, &extraRaw, &a.LastSeen,
		); err != nil {
			return nil, err
		}
		if companyID.Valid {
			cid := companyID.Int64
			a.CompanyID = &cid
		}
		if port.Valid {
			p := int(port.Int64)
			a.Port = &p
		}
		if statusCode.Valid {
			sc := int(statusCode.Int64)
			a.StatusCode = &sc
		}
		if contentLength.Valid {
			cl := contentLength.Int64
			a.ContentLength = &cl
		}
		// parse arrays
		if len(taskIDsRaw) > 0 {
			_ = json.Unmarshal(taskIDsRaw, &a.TaskIDs)
		}
		if len(boundDomainsRaw) > 0 {
			_ = json.Unmarshal(boundDomainsRaw, &a.BoundDomains)
		}
		if len(openPortsRaw) > 0 {
			_ = json.Unmarshal(openPortsRaw, &a.OpenPorts)
		}
		if len(recordValueRaw) > 0 {
			_ = json.Unmarshal(recordValueRaw, &a.RecordValue)
		}
		if len(techsRaw) > 0 {
			_ = json.Unmarshal(techsRaw, &a.Technologies)
		}
		if len(authRaw) > 0 {
			_ = json.Unmarshal(authRaw, &a.Auth)
		}
		if len(paramsRaw) > 0 {
			_ = json.Unmarshal(paramsRaw, &a.Params)
		}
		if len(extraRaw) > 0 {
			_ = json.Unmarshal(extraRaw, &a.Extra)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
