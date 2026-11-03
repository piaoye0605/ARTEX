package db

import (
	"fmt"
	"strings"
)

// QueryByType returns assets rows of a given type, newest first.
func (s *AssetStore) QueryByType(typ string, limit, offset int) ([]*Asset, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(`
SELECT id, type, company_id, array_to_json(task_ids)::text,
       COALESCE(domain,''), COALESCE(root_domain,''), COALESCE(ip,''),
       COALESCE(c_segment::text,''), port,
       COALESCE(icp,''), array_to_json(bound_domains)::text, array_to_json(open_ports)::text, COALESCE(record_type,''),
       array_to_json(record_value)::text, COALESCE(bundle_id,''), COALESCE(app_name,''),
       COALESCE(category,''), COALESCE(app_description,''), COALESCE(app_icp,''),
       COALESCE(url,''), COALESCE(service_type,''), COALESCE(service_name,''),
       COALESCE(favicon_mmh3,''), status_code, content_length,
       COALESCE(page_title,''), array_to_json(technologies)::text, array_to_json(auth)::text,
       COALESCE(method,''), array_to_json(params)::text, extra, last_seen::text
FROM assets
WHERE type = $1
ORDER BY last_seen DESC, id DESC
LIMIT $2 OFFSET $3`, typ, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssets(rows)
}

// CountByType returns the total number of assets of a type (for server-side pagination).
func (s *AssetStore) CountByType(typ string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM assets WHERE type = $1`, typ).Scan(&n)
	return n, err
}

// QueryByCompany returns assets for a company, optionally filtered by type.
// limit <= 0 means no limit.
func (s *AssetStore) QueryByCompany(companyID int64, typ string, limit, offset int) ([]*Asset, error) {
	q := `SELECT id, type, company_id, array_to_json(task_ids)::text,
       COALESCE(domain,''), COALESCE(root_domain,''), COALESCE(ip,''),
       COALESCE(c_segment::text,''), port,
       COALESCE(icp,''), array_to_json(bound_domains)::text, array_to_json(open_ports)::text, COALESCE(record_type,''),
       array_to_json(record_value)::text, COALESCE(bundle_id,''), COALESCE(app_name,''),
       COALESCE(category,''), COALESCE(app_description,''), COALESCE(app_icp,''),
       COALESCE(url,''), COALESCE(service_type,''), COALESCE(service_name,''),
       COALESCE(favicon_mmh3,''), status_code, content_length,
       COALESCE(page_title,''), array_to_json(technologies)::text, array_to_json(auth)::text,
       COALESCE(method,''), array_to_json(params)::text, extra, last_seen::text
FROM assets WHERE company_id = $1`
	args := []any{companyID}
	if typ != "" {
		args = append(args, typ)
		q += fmt.Sprintf(` AND type = $%d`, len(args))
	}
	q += pageClause(&args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssets(rows)
}

func (s *AssetStore) CountByCompany(companyID int64, typ string) (int, error) {
	q := `SELECT count(*) FROM assets WHERE company_id = $1`
	args := []any{companyID}
	if typ != "" {
		args = append(args, typ)
		q += fmt.Sprintf(` AND type = $%d`, len(args))
	}
	var n int
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

func pageClause(args *[]any, limit, offset int) string {
	q := ` ORDER BY last_seen DESC, id DESC`
	if limit > 0 {
		*args = append(*args, limit)
		q += fmt.Sprintf(` LIMIT $%d`, len(*args))
	}
	if offset > 0 {
		*args = append(*args, offset)
		q += fmt.Sprintf(` OFFSET $%d`, len(*args))
	}
	return q
}

// QueryByTask returns assets rows that have a given task_id in task_ids.
func (s *AssetStore) QueryByTask(taskID int64, typ string, limit, offset int) ([]*Asset, error) {
	q := `SELECT id, type, company_id, array_to_json(task_ids)::text,
       COALESCE(domain,''), COALESCE(root_domain,''), COALESCE(ip,''),
       COALESCE(c_segment::text,''), port,
       COALESCE(icp,''), array_to_json(bound_domains)::text, array_to_json(open_ports)::text, COALESCE(record_type,''),
       array_to_json(record_value)::text, COALESCE(bundle_id,''), COALESCE(app_name,''),
       COALESCE(category,''), COALESCE(app_description,''), COALESCE(app_icp,''),
       COALESCE(url,''), COALESCE(service_type,''), COALESCE(service_name,''),
       COALESCE(favicon_mmh3,''), status_code, content_length,
       COALESCE(page_title,''), array_to_json(technologies)::text, array_to_json(auth)::text,
       COALESCE(method,''), array_to_json(params)::text, extra, last_seen::text
FROM assets WHERE $1 = ANY(task_ids)`
	args := []any{taskID}
	if typ != "" {
		args = append(args, typ)
		q += fmt.Sprintf(` AND type = $%d`, len(args))
	}
	q += pageClause(&args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets, err := scanAssets(rows)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateTaskAssetSources(taskID, assets); err != nil {
		return nil, err
	}
	return assets, nil
}

func (s *AssetStore) CountByTask(taskID int64, typ string) (int, error) {
	q := `SELECT count(*) FROM assets WHERE $1 = ANY(task_ids)`
	args := []any{taskID}
	if typ != "" {
		args = append(args, typ)
		q += fmt.Sprintf(` AND type = $%d`, len(args))
	}
	var n int
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

func (s *AssetStore) CountsByTypeForTask(taskID int64) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT type, COUNT(*) FROM assets WHERE $1 = ANY(task_ids) GROUP BY type`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTypeCounts(rows)
}

// GetByIDs returns assets with the given ids (order preserved by id array order).
func (s *AssetStore) GetByIDs(ids []int64) ([]*Asset, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	sql := assetSelectCols + " WHERE id IN (" + strings.Join(placeholders, ",") + ") ORDER BY last_seen DESC"
	rows, err := s.db.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAssets(rows)
}

// CountsByType returns asset counts per type.
func (s *AssetStore) CountsByType() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT type, COUNT(*) FROM assets GROUP BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTypeCounts(rows)
}
