package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// CommandProvider uses one JSON request/response per invocation, without a
// shell. Its program must enforce generation and incarnation comparisons.
type CommandProvider struct{ Command []string }
type capacityRequest struct {
	Version    int    `json:"version"`
	Operation  string `json:"operation"`
	Generation string `json:"generation,omitempty"`
	Desired    int    `json:"desired,omitempty"`
	Node       *Node  `json:"node,omitempty"`
}
type capacityResponse struct {
	Version   int       `json:"version"`
	Inventory Inventory `json:"inventory"`
	Error     string    `json:"error,omitempty"`
}
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < b.limit {
		_, _ = b.Buffer.Write(p[:min(n, b.limit-b.Len())])
	}
	if b.Len() >= b.limit {
		return n, fmt.Errorf("provider output exceeds limit")
	}
	return n, nil
}
func (p CommandProvider) call(ctx context.Context, r capacityRequest) (Inventory, error) {
	if len(p.Command) == 0 {
		return Inventory{}, fmt.Errorf("capacity command is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	r.Version = 1
	data, _ := json.Marshal(r)
	cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
	cmd.Stdin = bytes.NewReader(data)
	cmd.WaitDelay = 5 * time.Second
	out, diagnostic := &limitedBuffer{limit: 4 << 20}, &limitedBuffer{limit: 8192}
	cmd.Stdout = out
	cmd.Stderr = diagnostic
	if err := cmd.Run(); err != nil {
		return Inventory{}, fmt.Errorf("capacity %s: %w: %s", r.Operation, err, strings.TrimSpace(diagnostic.String()))
	}
	var result capacityResponse
	dec := json.NewDecoder(out)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return Inventory{}, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Inventory{}, fmt.Errorf("capacity response must contain one JSON object")
	}
	if result.Version != 1 {
		return Inventory{}, fmt.Errorf("unsupported capacity protocol version")
	}
	if result.Error != "" {
		return Inventory{}, fmt.Errorf("capacity %s: %s", r.Operation, result.Error)
	}
	return result.Inventory, nil
}
func (p CommandProvider) Inventory(ctx context.Context) (Inventory, error) {
	return p.call(ctx, capacityRequest{Operation: "inventory"})
}
func (p CommandProvider) Grow(ctx context.Context, inv Inventory, n int) error {
	if n <= inv.Capacity {
		return fmt.Errorf("grow must increase capacity")
	}
	_, err := p.call(ctx, capacityRequest{Operation: "grow", Generation: inv.Generation, Desired: n})
	return err
}
func (p CommandProvider) Remove(ctx context.Context, inv Inventory, n Node) error {
	_, err := p.call(ctx, capacityRequest{Operation: "remove", Generation: inv.Generation, Node: &n})
	return err
}

// AzureVMSS uses the Azure CLI's noninteractive identity and ARM's conditional
// updates. Tokens stay out of candidate environments and diagnostic errors.
type AzureVMSS struct {
	ResourceID string
	CLI        []string
	Client     *http.Client
	Endpoint   string // ARM endpoint; empty uses public Azure
}

func (p AzureVMSS) token(ctx context.Context) (string, error) {
	argv := p.CLI
	if len(argv) == 0 {
		argv = []string{"az"}
	}
	cmd := exec.CommandContext(ctx, argv[0], append(append([]string{}, argv[1:]...), "account", "get-access-token", "--resource", "https://management.azure.com/", "--query", "accessToken", "--output", "tsv")...)
	cmd.WaitDelay = 5 * time.Second
	out := &limitedBuffer{limit: 32768}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("Azure CLI token acquisition: %w", err)
	}
	token := strings.TrimSpace(out.String())
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("Azure CLI returned invalid token")
	}
	return token, nil
}
func (p AzureVMSS) request(ctx context.Context, method, path, etag string, body any) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	token, err := p.token(ctx)
	if err != nil {
		return nil, "", err
	}
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = "https://management.azure.com"
	}
	target := endpoint + path
	u, err := url.Parse(target)
	if err != nil {
		return nil, "", err
	}
	q := u.Query()
	q.Set("api-version", "2024-03-01")
	u.RawQuery = q.Encode()
	var reader io.Reader
	if body != nil {
		data, e := json.Marshal(body)
		if e != nil {
			return nil, "", e
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	// Never follow a cloud redirect with the bearer credential.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("Azure ARM request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > 4<<20 {
		return nil, "", fmt.Errorf("Azure response exceeds limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Azure ARM %s: HTTP %d", method, resp.StatusCode)
	}
	return data, resp.Header.Get("ETag"), nil
}

type azureScaleSet struct {
	ETag string `json:"etag"`
	SKU  struct {
		Capacity int `json:"capacity"`
	} `json:"sku"`
	Properties struct {
		OrchestrationMode string `json:"orchestrationMode"`
		ProvisioningState string `json:"provisioningState"`
	} `json:"properties"`
}

func (p AzureVMSS) Inventory(ctx context.Context) (Inventory, error) {
	data, etag, err := p.request(ctx, "GET", p.ResourceID, "", nil)
	if err != nil {
		return Inventory{}, err
	}
	var set azureScaleSet
	if err = json.Unmarshal(data, &set); err != nil {
		return Inventory{}, err
	}
	if set.Properties.OrchestrationMode != "" && set.Properties.OrchestrationMode != "Uniform" {
		return Inventory{}, fmt.Errorf("Azure capacity requires a Uniform VM scale set")
	}
	if etag == "" {
		etag = set.ETag
	}
	if etag == "" {
		return Inventory{}, fmt.Errorf("Azure inventory has no ETag; refusing unconditional scaling")
	}
	inv := Inventory{Generation: etag, Capacity: set.SKU.Capacity, Busy: set.Properties.ProvisioningState != "Succeeded"}
	path := p.ResourceID + "/virtualMachines"
	for page := 0; path != ""; page++ {
		if page >= 100 {
			return Inventory{}, fmt.Errorf("Azure inventory pagination limit")
		}
		data, _, err = p.request(ctx, "GET", path, "", nil)
		if err != nil {
			return Inventory{}, err
		}
		var list struct {
			Value []struct {
				InstanceID string `json:"instanceId"`
				Properties struct {
					VMID              string `json:"vmId"`
					ProvisioningState string `json:"provisioningState"`
					OSProfile         struct {
						ComputerName string `json:"computerName"`
					} `json:"osProfile"`
				} `json:"properties"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err = json.Unmarshal(data, &list); err != nil {
			return Inventory{}, err
		}
		for _, vm := range list.Value {
			inv.Nodes = append(inv.Nodes, Node{ID: vm.InstanceID, Incarnation: vm.Properties.VMID, Worker: vm.Properties.OSProfile.ComputerName})
			if vm.Properties.ProvisioningState != "Succeeded" {
				inv.Busy = true
			}
		}
		path = ""
		if list.NextLink != "" {
			u, e := url.Parse(list.NextLink)
			if e != nil {
				return Inventory{}, e
			}
			endpoint := p.Endpoint
			if endpoint == "" {
				endpoint = "https://management.azure.com"
			}
			base, _ := url.Parse(endpoint)
			if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Path != p.ResourceID+"/virtualMachines" {
				return Inventory{}, fmt.Errorf("unexpected Azure pagination URL")
			}
			path = u.RequestURI()
		}
	}
	return inv, validateInventory(inv)
}
func (p AzureVMSS) Grow(ctx context.Context, inv Inventory, n int) error {
	if n <= inv.Capacity || inv.Generation == "" {
		return fmt.Errorf("Azure growth requires a newer capacity and ETag")
	}
	_, _, err := p.request(ctx, "PATCH", p.ResourceID, inv.Generation, map[string]any{"sku": map[string]any{"capacity": n}})
	return err
}
func (p AzureVMSS) Remove(ctx context.Context, inv Inventory, n Node) error {
	// Uniform instance IDs can be reused. Check the immutable VM ID immediately
	// before requesting deletion, and retain the scale set generation guard.
	current, err := p.Inventory(ctx)
	if err != nil {
		return err
	}
	if current.Generation != inv.Generation {
		return fmt.Errorf("Azure pool changed before removal")
	}
	found := false
	for _, node := range current.Nodes {
		if node.ID == n.ID {
			if node.Incarnation != n.Incarnation || node.Worker != n.Worker {
				return fmt.Errorf("Azure instance was replaced")
			}
			found = true
		}
	}
	if !found {
		return nil
	}
	if strings.ContainsAny(n.ID, "/\\?#") || n.ID == "" {
		return errors.New("invalid Azure instance ID")
	}
	_, _, err = p.request(ctx, "POST", p.ResourceID+"/delete", inv.Generation, map[string]any{"instanceIds": []string{n.ID}})
	return err
}
