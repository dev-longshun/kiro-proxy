package legacy

import "fmt"

// QueryTotalSummary returns overall usage summary from DB.
func (d *Database) QueryTotalSummary() (*UsageSummary, error) {
	row := d.db.QueryRow(`SELECT
		COALESCE(COUNT(*),0),
		COALESCE(SUM(input_tokens),0),
		COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(CASE WHEN success=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0),
		COALESCE(AVG(duration_ms),0)
		FROM usage_records`)
	s := &UsageSummary{}
	err := row.Scan(&s.TotalRequests, &s.TotalInput, &s.TotalOutput, &s.TotalTokens,
		&s.SuccessCount, &s.ErrorCount, &s.AvgDurationMs)
	return s, err
}

// QuerySummaryByAccount returns usage summary grouped by account email.
func (d *Database) QuerySummaryByAccount() (map[string]*UsageSummary, error) {
	return d.querySummaryGroupBy("account_email")
}

// QuerySummaryByModel returns usage summary grouped by model.
func (d *Database) QuerySummaryByModel() (map[string]*UsageSummary, error) {
	return d.querySummaryGroupBy("model")
}

// QuerySummaryByKey returns usage summary grouped by API key.
func (d *Database) QuerySummaryByKey() (map[string]*UsageSummary, error) {
	return d.querySummaryGroupBy("api_key")
}

func (d *Database) querySummaryGroupBy(col string) (map[string]*UsageSummary, error) {
	query := fmt.Sprintf(`SELECT COALESCE(%s,'_unknown_'),
		COUNT(*),
		COALESCE(SUM(input_tokens),0),
		COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(CASE WHEN success=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0),
		COALESCE(AVG(duration_ms),0)
		FROM usage_records GROUP BY %s`, col, col)
	rows, err := d.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]*UsageSummary)
	for rows.Next() {
		var key string
		s := &UsageSummary{}
		if err := rows.Scan(&key, &s.TotalRequests, &s.TotalInput, &s.TotalOutput, &s.TotalTokens,
			&s.SuccessCount, &s.ErrorCount, &s.AvgDurationMs); err != nil {
			continue
		}
		if key == "" {
			key = "_unknown_"
		}
		result[key] = s
	}
	return result, nil
}
