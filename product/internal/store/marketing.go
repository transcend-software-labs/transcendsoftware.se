package store

import (
	"context"
	"sort"
	"time"
)

type marketingDailyKey struct {
	Day      string
	Event    string
	Source   string
	Medium   string
	Campaign string
}

func marketingKey(event MarketingEvent) marketingDailyKey {
	return marketingDailyKey{
		Day: event.OccurredAt.UTC().Format(time.DateOnly), Event: event.Kind,
		Source: event.Source, Medium: event.Medium, Campaign: event.Campaign,
	}
}

func (m *Memory) RecordMarketingEvent(_ context.Context, event MarketingEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marketing[marketingKey(event)]++
	return nil
}

func (m *Memory) MarketingFunnel(_ context.Context, since time.Time) (MarketingFunnel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out MarketingFunnel
	sinceDay := since.UTC().Format(time.DateOnly)
	sources := map[[3]string]*MarketingSource{}
	for key, count := range m.marketing {
		if key.Day < sinceDay {
			continue
		}
		dims := [3]string{key.Source, key.Medium, key.Campaign}
		src := sources[dims]
		if src == nil {
			src = &MarketingSource{Source: key.Source, Medium: key.Medium, Campaign: key.Campaign}
			sources[dims] = src
		}
		switch key.Event {
		case MarketingLandingView:
			out.LandingViews += count
			src.LandingViews += count
		case MarketingStart:
			out.Starts += count
			src.Starts += count
		case MarketingSignupView:
			out.SignupViews += count
			src.SignupViews += count
		}
	}

	for _, u := range m.users {
		if !u.CreatedAt.Before(since) {
			out.Signups++
			if u.Approved() {
				out.Approved++
			}
		}
	}
	briefUsers, previewUsers, paidUsers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range m.projects {
		if p.CreatedAt.Before(since) {
			continue
		}
		briefUsers[p.UserID] = true
		if p.PreviewURL != "" {
			previewUsers[p.UserID] = true
		}
		if p.Paid {
			paidUsers[p.UserID] = true
		}
	}
	out.Briefs, out.Previews, out.Paid = len(briefUsers), len(previewUsers), len(paidUsers)

	for _, src := range sources {
		out.Sources = append(out.Sources, *src)
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		a, b := out.Sources[i], out.Sources[j]
		if a.Starts != b.Starts {
			return a.Starts > b.Starts
		}
		if a.SignupViews != b.SignupViews {
			return a.SignupViews > b.SignupViews
		}
		if a.LandingViews != b.LandingViews {
			return a.LandingViews > b.LandingViews
		}
		return a.Source+a.Medium+a.Campaign < b.Source+b.Medium+b.Campaign
	})
	if len(out.Sources) > 10 {
		out.Sources = out.Sources[:10]
	}
	return out, nil
}

func (p *Postgres) RecordMarketingEvent(ctx context.Context, event MarketingEvent) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO marketing_daily (day, event, source, medium, campaign, count)
		 VALUES ($1,$2,$3,$4,$5,1)
		 ON CONFLICT (day, event, source, medium, campaign)
		 DO UPDATE SET count = marketing_daily.count + 1`,
		event.OccurredAt.UTC(), event.Kind, event.Source, event.Medium, event.Campaign)
	return err
}

func (p *Postgres) MarketingFunnel(ctx context.Context, since time.Time) (MarketingFunnel, error) {
	var out MarketingFunnel
	err := p.pool.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT sum(count) FROM marketing_daily WHERE day >= $1::date AND event = $2), 0),
		  COALESCE((SELECT sum(count) FROM marketing_daily WHERE day >= $1::date AND event = $3), 0),
		  COALESCE((SELECT sum(count) FROM marketing_daily WHERE day >= $1::date AND event = $4), 0),
		  (SELECT count(*) FROM users WHERE created_at >= $1),
		  (SELECT count(DISTINCT user_id) FROM projects WHERE created_at >= $1),
		  (SELECT count(*) FROM users WHERE created_at >= $1 AND approved_at IS NOT NULL),
		  (SELECT count(DISTINCT user_id) FROM projects WHERE created_at >= $1 AND preview_url <> ''),
		  (SELECT count(DISTINCT user_id) FROM projects WHERE created_at >= $1 AND paid = true)`,
		since, MarketingLandingView, MarketingStart, MarketingSignupView).
		Scan(&out.LandingViews, &out.Starts, &out.SignupViews, &out.Signups,
			&out.Briefs, &out.Approved, &out.Previews, &out.Paid)
	if err != nil {
		return MarketingFunnel{}, err
	}

	rows, err := p.pool.Query(ctx, `
		SELECT source, medium, campaign,
		       COALESCE(sum(count) FILTER (WHERE event = $2), 0),
		       COALESCE(sum(count) FILTER (WHERE event = $3), 0),
		       COALESCE(sum(count) FILTER (WHERE event = $4), 0)
		  FROM marketing_daily
		 WHERE day >= $1::date
		 GROUP BY source, medium, campaign
		 ORDER BY 5 DESC, 6 DESC, 4 DESC
		 LIMIT 10`, since, MarketingLandingView, MarketingStart, MarketingSignupView)
	if err != nil {
		return MarketingFunnel{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var src MarketingSource
		if err := rows.Scan(&src.Source, &src.Medium, &src.Campaign,
			&src.LandingViews, &src.Starts, &src.SignupViews); err != nil {
			return MarketingFunnel{}, err
		}
		out.Sources = append(out.Sources, src)
	}
	return out, rows.Err()
}
