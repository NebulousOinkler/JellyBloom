package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PublicInfo is the unauthenticated /System/Info/Public response.
type PublicInfo struct {
	ServerName             string
	Version                string
	ProductName            string
	Id                     string
	LocalAddress           string
	StartupWizardCompleted bool
}

// QueryResult is Jellyfin's paged list envelope. Items are kept as raw DTO JSON
// so the index can store them verbatim.
type QueryResult struct {
	Items            []json.RawMessage
	TotalRecordCount int
	StartIndex       int
}

// ItemsQuery is the subset of /Items parameters JellyBloom uses. Extra is
// merged in last for anything not modeled here.
type ItemsQuery struct {
	ParentID         string
	IDs              []string
	Recursive        bool
	IncludeItemTypes []string
	Fields           []string
	SortBy           []string
	StartIndex       int
	Limit            int
	Extra            url.Values
}

func (q ItemsQuery) values(userID string) url.Values {
	v := url.Values{}
	v.Set("userId", userID)
	v.Set("enableTotalRecordCount", "true")
	if q.ParentID != "" {
		v.Set("parentId", q.ParentID)
	}
	if len(q.IDs) > 0 {
		v.Set("ids", strings.Join(q.IDs, ","))
	}
	if q.Recursive {
		v.Set("recursive", "true")
	}
	if len(q.IncludeItemTypes) > 0 {
		v.Set("includeItemTypes", strings.Join(q.IncludeItemTypes, ","))
	}
	if len(q.Fields) > 0 {
		v.Set("fields", strings.Join(q.Fields, ","))
	}
	if len(q.SortBy) > 0 {
		v.Set("sortBy", strings.Join(q.SortBy, ","))
	}
	if q.StartIndex > 0 {
		v.Set("startIndex", strconv.Itoa(q.StartIndex))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	for k, vs := range q.Extra {
		v[k] = vs
	}
	return v
}

// PublicInfo fetches server identity without authenticating.
func (c *Client) PublicInfo(ctx context.Context) (*PublicInfo, error) {
	var info PublicInfo
	if err := c.send(ctx, http.MethodGet, "/System/Info/Public", nil, nil, "", &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Ping checks reachability and returns the round-trip time of one
// /System/Info/Public request (no retries, so failures surface immediately).
func (c *Client) Ping(ctx context.Context) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/System/Info/Public", nil), nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	if err := c.handle(resp, http.MethodGet, "/System/Info/Public", nil); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// Views lists the libraries visible to the upstream user.
func (c *Client) Views(ctx context.Context) (*QueryResult, error) {
	if err := c.ensureLogin(ctx, ""); err != nil {
		return nil, err
	}
	var res QueryResult
	if err := c.get(ctx, "/UserViews", url.Values{"userId": {c.UserID()}}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Items runs one /Items query as the upstream user.
func (c *Client) Items(ctx context.Context, q ItemsQuery) (*QueryResult, error) {
	return c.query(ctx, "/Items", q)
}

// Artists runs one /Artists query. Music artists are not children of the
// library tree, so recursive /Items listings do not include them.
func (c *Client) Artists(ctx context.Context, q ItemsQuery) (*QueryResult, error) {
	return c.query(ctx, "/Artists", q)
}

func (c *Client) query(ctx context.Context, path string, q ItemsQuery) (*QueryResult, error) {
	if err := c.ensureLogin(ctx, ""); err != nil {
		return nil, err
	}
	var res QueryResult
	if err := c.get(ctx, path, q.values(c.UserID()), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// EachItemPage pages through q, calling fn for each non-empty page. q.StartIndex
// is the starting offset and pageSize replaces q.Limit. It stops when fn returns
// an error, the upstream returns an empty page, or TotalRecordCount is reached.
func (c *Client) EachItemPage(ctx context.Context, q ItemsQuery, pageSize int, fn func(*QueryResult) error) error {
	return c.eachPage(ctx, "/Items", q, pageSize, fn)
}

// EachArtistPage is EachItemPage over /Artists.
func (c *Client) EachArtistPage(ctx context.Context, q ItemsQuery, pageSize int, fn func(*QueryResult) error) error {
	return c.eachPage(ctx, "/Artists", q, pageSize, fn)
}

func (c *Client) eachPage(ctx context.Context, path string, q ItemsQuery, pageSize int, fn func(*QueryResult) error) error {
	if pageSize <= 0 {
		pageSize = 500
	}
	q.Limit = pageSize
	for {
		page, err := c.query(ctx, path, q)
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			return nil
		}
		if err := fn(page); err != nil {
			return err
		}
		q.StartIndex += len(page.Items)
		if q.StartIndex >= page.TotalRecordCount {
			return nil
		}
	}
}
