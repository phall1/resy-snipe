package resy

import (
	"context"
	"net/url"
	"strings"
)

// GetForTest exercises the unexported do() through a stable test
// surface. R2/R3/R4 will call do() from real endpoint methods; until
// then this is the only caller.
func GetForTest(c *Client, ctx context.Context, path string, query url.Values, authToken string) (*Response, []byte, error) {
	body, resp, err := c.do(ctx, "GET", path, query, nil, authToken)
	return resp, body, err
}

// PostForTest is the POST counterpart for the test surface.
func PostForTest(c *Client, ctx context.Context, path string, form url.Values, authToken string) (*Response, []byte, error) {
	body, resp, err := c.do(ctx, "POST", path, nil, strings.NewReader(form.Encode()), authToken)
	return resp, body, err
}
