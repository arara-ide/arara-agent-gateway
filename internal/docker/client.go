package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const apiVersion = "/v1.43"

var ErrNotFound = errors.New("docker object not found")

type Client struct {
	socket string
	http   *http.Client
}

func NewClient(socket string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	return &Client{
		socket: socket,
		http:   &http.Client{Transport: &http.Transport{DialContext: dial, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}},
	}
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("docker api %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	u := "http://docker" + apiVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		var e struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		return &apiError{Status: resp.StatusCode, Message: e.Message}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	body := map[string]any{
		"Name":   name,
		"Driver": "bridge",
		"Options": map[string]string{
			"com.docker.network.bridge.enable_icc": "false",
		},
		"Labels": map[string]string{"aag.managed": "true"},
	}
	err = c.do(ctx, http.MethodPost, "/networks/create", nil, body, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		return nil
	}
	return err
}

func (c *Client) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), url.Values{"force": {"true"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	mounts := make([]map[string]any, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		mounts = append(mounts, map[string]any{"Type": "volume", "Source": m.Volume, "Target": m.Target})
	}
	host := map[string]any{
		"Mounts":         mounts,
		"Memory":         spec.Memory,
		"MemorySwap":     spec.Memory,
		"NanoCpus":       spec.NanoCPUs,
		"PidsLimit":      spec.Pids,
		"CapDrop":        []string{"ALL"},
		"SecurityOpt":    []string{"no-new-privileges:true"},
		"ReadonlyRootfs": true,
		"Tmpfs":          spec.Tmpfs,
		"NetworkMode":    spec.Network,
		"Init":           true,
		"RestartPolicy":  map[string]string{"Name": "no"},
		"LogConfig":      map[string]any{"Type": "none"},
		"Privileged":     false,
	}
	if spec.Runtime != "" {
		host["Runtime"] = spec.Runtime
	}
	body := map[string]any{
		"Image":      spec.Image,
		"User":       spec.User,
		"Env":        spec.Env,
		"Labels":     spec.Labels,
		"Cmd":        spec.Cmd,
		"Hostname":   "agent",
		"WorkingDir": "/home/agent",
		"HostConfig": host,
	}
	var out struct{ Id string }
	err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {spec.Name}}, body, &out)
	return out.Id, err
}

func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil)
}

func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"true"}, "v": {"false"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerInfo, error) {
	var out struct {
		Id     string
		Name   string
		State  struct{ Running bool }
		Config struct{ Labels map[string]string }
	}
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out); err != nil {
		return ContainerInfo{}, err
	}
	return ContainerInfo{ID: out.Id, Name: out.Name, Running: out.State.Running, Labels: out.Config.Labels}, nil
}

func (c *Client) ListContainers(ctx context.Context, label string) ([]ContainerInfo, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []struct {
		Id     string
		Names  []string
		State  string
		Labels map[string]string
	}
	if err := c.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"true"}, "filters": {string(filters)}}, nil, &out); err != nil {
		return nil, err
	}
	res := make([]ContainerInfo, 0, len(out))
	for _, o := range out {
		name := ""
		if len(o.Names) > 0 {
			name = o.Names[0]
		}
		res = append(res, ContainerInfo{ID: o.Id, Name: name, Running: o.State == "running", Labels: o.Labels})
	}
	return res, nil
}

func (c *Client) Exec(ctx context.Context, spec ExecSpec) (Process, error) {
	create := map[string]any{
		"AttachStdin":  spec.Stdin,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          spec.Tty,
		"Cmd":          spec.Cmd,
		"Env":          spec.Env,
		"WorkingDir":   spec.WorkDir,
	}
	if spec.Tty {
		create["ConsoleSize"] = []int{60, 1000}
	}
	var created struct{ Id string }
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(spec.Container)+"/exec", nil, create, &created); err != nil {
		return nil, err
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"Detach": false, "Tty": spec.Tty})
	req := fmt.Sprintf("POST %s/exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n", apiVersion, created.Id, len(payload))
	if _, err := conn.Write(append([]byte(req), payload...)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, &apiError{Status: resp.StatusCode, Message: "exec start failed"}
	}

	p := &process{client: c, execID: created.Id, conn: conn, done: make(chan struct{})}
	outR, outW := io.Pipe()
	p.stdout = outR
	go func() {
		defer close(p.done)
		var err error
		if spec.Tty {
			_, err = io.Copy(outW, br)
		} else {
			err = demux(br, outW, &p.stderr)
		}
		outW.CloseWithError(err)
	}()
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-p.done:
		}
	}()
	return p, nil
}

func demux(r io.Reader, stdout, stderr io.Writer) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		dst := stdout
		if header[0] == 2 {
			dst = stderr
		}
		if _, err := io.CopyN(dst, r, size); err != nil {
			return err
		}
	}
}

type process struct {
	client *Client
	execID string
	conn   net.Conn
	stdout io.Reader
	stderr Tail
	done   chan struct{}
	once   sync.Once
}

func (p *process) Stdout() io.Reader  { return p.stdout }
func (p *process) StderrTail() string { return p.stderr.String() }

func (p *process) Write(b []byte) (int, error) { return p.conn.Write(b) }

func (p *process) CloseStdin() error {
	if cw, ok := p.conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (p *process) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
	case <-ctx.Done():
		p.Close()
		<-p.done
	}
	inspectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 50; i++ {
		var out struct {
			Running  bool
			ExitCode int
		}
		if err := p.client.do(inspectCtx, http.MethodGet, "/exec/"+p.execID+"/json", nil, nil, &out); err != nil {
			return -1, err
		}
		if !out.Running {
			return out.ExitCode, ctx.Err()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return -1, errors.New("exec still running after stream closed")
}

func (p *process) Close() error {
	var err error
	p.once.Do(func() { err = p.conn.Close() })
	return err
}
