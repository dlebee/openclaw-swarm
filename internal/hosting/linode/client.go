package linode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gluwa/openclaw-swarm2/internal/hosting"
)

// Provider implements hosting.Provider for Linode.
type Provider struct {
	token string
	http  *http.Client
}

// NewProvider returns a Linode API client.
func NewProvider(token string) *Provider {
	return &Provider{
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *Provider) Kind() string { return hosting.KindLinode }

func (p *Provider) do(ctx context.Context, method, path string, body any) (map[string]any, error) {
	return p.doRequest(ctx, method, path, body, nil)
}

func (p *Provider) doRequest(ctx context.Context, method, path string, body any, xFilter []byte) (map[string]any, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.linode.com/v4"+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if len(xFilter) > 0 {
		req.Header.Set("X-Filter", string(xFilter))
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(raw))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("linode API %s %s: status %d: %v", method, path, resp.StatusCode, result)
	}
	return result, nil
}

// CreateInstance implements hosting.Provider.
func (p *Provider) CreateInstance(ctx context.Context, opts hosting.CreateInstanceOpts) (*hosting.Instance, error) {
	result, err := p.do(ctx, "POST", "/linode/instances", map[string]any{
		"type":            opts.SKU,
		"region":          opts.Region,
		"image":           opts.Image,
		"label":           truncateLabel(opts.Label),
		"root_pass":       opts.RootPass,
		"authorized_keys": opts.PublicKeys,
		"tags":            opts.Tags,
	})
	if err != nil {
		return nil, err
	}
	return instanceFromResult(result), nil
}

// DeleteInstance implements hosting.Provider.
func (p *Provider) DeleteInstance(ctx context.Context, resourceID string) error {
	_, err := p.do(ctx, "DELETE", fmt.Sprintf("/linode/instances/%s", resourceID), nil)
	return err
}

// WaitRunning implements hosting.Provider.
func (p *Provider) WaitRunning(ctx context.Context, resourceID string) (*hosting.Instance, error) {
	for {
		result, err := p.do(ctx, "GET", fmt.Sprintf("/linode/instances/%s", resourceID), nil)
		if err != nil {
			return nil, err
		}
		inst := instanceFromResult(result)
		if inst.Status == "running" && inst.PublicIPv4 != "" {
			return inst, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// ListByTag implements hosting.Provider — lists instances tagged with tag (e.g. claws/<prefix>).
// Results are filtered so the instance's tag list actually contains tag (API tag filter is not fully reliable).
func (p *Provider) ListByTag(ctx context.Context, tag string) ([]hosting.Instance, error) {
	if tag == "" {
		return nil, nil
	}
	var all []hosting.Instance
	page := 1
	for {
		path := fmt.Sprintf("/linode/instances?page=%d&page_size=100&tags=%s", page, url.QueryEscape(tag))
		result, err := p.do(ctx, "GET", path, nil)
		if err != nil {
			return nil, err
		}
		data, _ := result["data"].([]any)
		for _, d := range data {
			m, ok := d.(map[string]any)
			if !ok {
				continue
			}
			inst := instanceFromResult(m)
			all = append(all, *inst)
		}
		pages := intField(result, "pages")
		if page >= pages || len(data) == 0 {
			break
		}
		page++
	}
	return instancesWithExactTag(all, tag), nil
}

func instancesWithExactTag(instances []hosting.Instance, tag string) []hosting.Instance {
	if tag == "" {
		return nil
	}
	var out []hosting.Instance
	for i := range instances {
		for _, t := range instances[i].Tags {
			if t == tag {
				out = append(out, instances[i])
				break
			}
		}
	}
	return out
}

func instanceFromResult(m map[string]any) *hosting.Instance {
	ipv4s, _ := m["ipv4"].([]any)
	ip := ""
	if len(ipv4s) > 0 {
		ip, _ = ipv4s[0].(string)
	}
	var tags []string
	if rawTags, ok := m["tags"].([]any); ok {
		for _, t := range rawTags {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	}
	return &hosting.Instance{
		Provider:   hosting.KindLinode,
		ResourceID: strconv.Itoa(int(floatField(m, "id"))),
		Label:      stringField(m, "label"),
		Region:     stringField(m, "region"),
		PublicIPv4: ip,
		Status:     stringField(m, "status"),
		Tags:       tags,
	}
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func floatField(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

func intField(m map[string]any, key string) int {
	return int(floatField(m, key))
}

// truncateLabel keeps Linode label length within API limits.
func truncateLabel(label string) string {
	if len(label) <= 64 {
		return label
	}
	return label[:64]
}
