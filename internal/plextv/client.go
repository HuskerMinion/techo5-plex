package plextv

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/HuskerMinion/techo5-plex/internal/netguard"
)

// Plex's own addresses. Fields of Client, so tests can point them at a stand-in.
const (
	DefaultAPI     = "https://plex.tv/api/v2"
	DefaultDevices = "https://plex.tv"
	// DefaultResources is where the account's servers are listed, with their addresses.
	DefaultResources = "https://clients.plex.tv/api/v2"
	LinkURL          = "https://plex.tv/link"
)

// mostBody bounds what plex.tv may answer with.
const mostBody = 1 << 20

// Client talks to plex.tv.
type Client struct {
	ID      *Identity
	API     string // https://plex.tv/api/v2
	Devices string // https://plex.tv (devices.xml and /devices/{id})
	// Resources is https://clients.plex.tv/api/v2 (/resources).
	Resources string
	HTTP      *http.Client
}

// NewClient is a client for the real plex.tv.
func NewClient(id *Identity) *Client {
	return &Client{ID: id, API: DefaultAPI, Devices: DefaultDevices, Resources: DefaultResources,
		HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// StatusError is plex.tv answering something other than 2xx. Only the status goes in the message:
// what plex.tv says back can echo the request.
type StatusError struct{ Code int }

func (e StatusError) Error() string { return fmt.Sprintf("plex.tv answered %d", e.Code) }

// Pin is plex.tv's answer to a PIN: created with a code, and later with a token in it. The id is raw
// because Plex has written it as a number and as a string.
type Pin struct {
	ID        json.RawMessage `json:"id"`
	Code      string          `json:"code"`
	AuthToken string          `json:"authToken"`
	ExpiresIn int             `json:"expiresIn"`
}

// PinID is the id as a string.
func (p Pin) PinID() string { return idString(p.ID) }

func (c *Client) do(ctx context.Context, method, u, token string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	c.ID.Apply(req)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, scrub(err)
	}
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, StatusError{resp.StatusCode}
	}
	return resp, nil
}

// scrub drops the URL from a transport error: none of the URLs here carry a token, but the rule is
// simpler kept everywhere.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// CreatePin asks for a code. The plain four-character one, not strong=true: that is a long token for
// a link, and plex.tv/link will not take it.
func (c *Client) CreatePin(ctx context.Context) (Pin, error) {
	var p Pin
	resp, err := c.do(ctx, http.MethodPost, c.API+"/pins", "", "")
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, mostBody)).Decode(&p); err != nil {
		return p, fmt.Errorf("plex.tv's answer was not readable: %w", err)
	}
	if p.Code == "" || p.PinID() == "" {
		return p, errors.New("plex.tv made no code")
	}
	return p, nil
}

// CheckPin reads a PIN back; AuthToken is set once the code was entered.
func (c *Client) CheckPin(ctx context.Context, id string) (Pin, error) {
	var p Pin
	resp, err := c.do(ctx, http.MethodGet, c.API+"/pins/"+url.PathEscape(id), "", "")
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(io.LimitReader(resp.Body, mostBody)).Decode(&p)
	return p, err
}

// User is whose token this is: the name, and the account number.
func (c *Client) User(ctx context.Context, token string) (name, id string, err error) {
	resp, err := c.do(ctx, http.MethodGet, c.API+"/user", token, "")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var u struct {
		ID       json.RawMessage `json:"id"`
		Username string          `json:"username"`
		Title    string          `json:"title"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, mostBody)).Decode(&u); err != nil {
		return "", "", err
	}
	if u.Username == "" {
		u.Username = u.Title
	}
	return u.Username, idString(u.ID), nil
}

// DeviceID is this device's number in the account's device list, or "" when the account has no record
// of it yet. A device is published to by that number: a PUT against the client identifier is answered
// 200 and changes nothing.
func (c *Client) DeviceID(ctx context.Context, token string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, c.Devices+"/devices.xml", token, "application/xml")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var list struct {
		Devices []struct {
			ID               string `xml:"id,attr"`
			ClientIdentifier string `xml:"clientIdentifier,attr"`
		} `xml:"Device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, mostBody)).Decode(&list); err != nil {
		return "", fmt.Errorf("the device list was not readable: %w", err)
	}
	for _, d := range list.Devices {
		if d.ClientIdentifier == c.ID.ClientID() {
			return d.ID, nil
		}
	}
	return "", nil
}

// Register publishes the player's addresses against its number. One request carries every address:
// publishing replaces the connections the account had.
func (c *Client) Register(ctx context.Context, token, deviceID string, uris []string) error {
	q := url.Values{}
	for _, u := range uris {
		q.Add("Connection[][uri]", u)
	}
	resp, err := c.do(ctx, http.MethodPut, c.Devices+"/devices/"+url.PathEscape(deviceID)+"?"+q.Encode(), token, "application/xml")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Unregister takes the device out of the account's list. Best effort.
func (c *Client) Unregister(ctx context.Context, token string) error {
	id, err := c.DeviceID(ctx, token)
	if err != nil || id == "" {
		return err
	}
	resp, err := c.do(ctx, http.MethodDelete, c.Devices+"/devices/"+url.PathEscape(id)+".xml", token, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// idString is a JSON id as a string, whether Plex wrote a number or a string.
func idString(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

// LocalServers is the home-network addresses of the Plex Media Servers the account can reach: each
// resource that provides "server", and its connections marked local. A connection's address is an
// address, or with includeHttps a plex.direct name that has the address in it (netguard.PlexDirectIP).
// Which of them are worth announcing to is the caller's to decide.
func (c *Client) LocalServers(ctx context.Context, token string) ([]net.IP, error) {
	resp, err := c.do(ctx, http.MethodGet, c.Resources+"/resources?includeHttps=1", token, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []struct {
		Provides    string `json:"provides"`
		Connections []struct {
			Address string `json:"address"`
			URI     string `json:"uri"`
			Local   bool   `json:"local"`
			Relay   bool   `json:"relay"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, mostBody)).Decode(&list); err != nil {
		return nil, fmt.Errorf("the account's servers were not readable: %w", err)
	}
	var out []net.IP
	for _, r := range list {
		if !provides(r.Provides, "server") {
			continue
		}
		for _, cn := range r.Connections {
			if !cn.Local || cn.Relay {
				continue
			}
			ip := net.ParseIP(cn.Address)
			if ip == nil {
				ip = netguard.PlexDirectIP(cn.Address)
			}
			if ip == nil {
				if u, err := url.Parse(cn.URI); err == nil {
					ip = netguard.PlexDirectIP(u.Hostname())
				}
			}
			if ip != nil {
				out = append(out, ip)
			}
		}
	}
	return out, nil
}

// ServerAccess is the access token plex.tv holds for the account on the server machineID, and the
// host names and addresses the account lists for that server. A token a controller passes with a cast
// may be a transient one the server will not take for changes (a part's chosen tracks: 403); this one
// it takes. The caller sends it only to one of hosts, so a controller cannot have it sent elsewhere.
func (c *Client) ServerAccess(ctx context.Context, token, machineID string) (access string, hosts []string, err error) {
	resp, err := c.do(ctx, http.MethodGet, c.Resources+"/resources?includeHttps=1", token, "application/json")
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	var list []struct {
		ClientIdentifier string `json:"clientIdentifier"`
		Provides         string `json:"provides"`
		AccessToken      string `json:"accessToken"`
		Connections      []struct {
			Address string `json:"address"`
			URI     string `json:"uri"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, mostBody)).Decode(&list); err != nil {
		return "", nil, fmt.Errorf("the account's servers were not readable: %w", err)
	}
	for _, r := range list {
		if r.ClientIdentifier != machineID || !provides(r.Provides, "server") || r.AccessToken == "" {
			continue
		}
		for _, cn := range r.Connections {
			if cn.Address != "" {
				hosts = append(hosts, strings.ToLower(cn.Address))
			}
			if u, err := url.Parse(cn.URI); err == nil && u.Hostname() != "" {
				hosts = append(hosts, strings.ToLower(u.Hostname()))
			}
		}
		return r.AccessToken, hosts, nil
	}
	return "", nil, errors.New("the account does not list that server")
}

// provides is whether a comma-separated provides list names what.
func provides(list, what string) bool {
	for _, p := range strings.Split(list, ",") {
		if strings.TrimSpace(p) == what {
			return true
		}
	}
	return false
}

// Server is a Plex Media Server the linked account can reach on the home network: its name, its
// machine identifier, the account's access token on it, and the addresses to ask it at, as plain HTTP
// to an address (the device's DNS often refuses plex.direct names, as rendererServer in companion
// says).
type Server struct {
	Name    string
	Machine string
	Token   string
	Owned   bool
	Bases   []string // http://192.168.1.10:32400
}

// Servers is the account's servers with a connection on the home network, the account's own first.
// Relayed and remote connections are left out: voice plays only from a server in the house.
func (c *Client) Servers(ctx context.Context, token string) ([]Server, error) {
	resp, err := c.do(ctx, http.MethodGet, c.Resources+"/resources?includeHttps=1", token, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []struct {
		Name             string `json:"name"`
		ClientIdentifier string `json:"clientIdentifier"`
		Provides         string `json:"provides"`
		AccessToken      string `json:"accessToken"`
		Owned            bool   `json:"owned"`
		// PublicAddressMatches: the server is behind the same public address as this device, so its
		// "local" connections are on this network. A server shared from another household marks its
		// own network's addresses local too, and those are never reachable from here.
		PublicAddressMatches bool `json:"publicAddressMatches"`
		Connections          []struct {
			Address string `json:"address"`
			Port    int    `json:"port"`
			URI     string `json:"uri"`
			Local   bool   `json:"local"`
			Relay   bool   `json:"relay"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, mostBody)).Decode(&list); err != nil {
		return nil, fmt.Errorf("the account's servers were not readable: %w", err)
	}
	var own, shared []Server
	for _, r := range list {
		if !provides(r.Provides, "server") || r.AccessToken == "" || r.ClientIdentifier == "" {
			continue
		}
		if !r.Owned && !r.PublicAddressMatches {
			continue // shared from another network: its local addresses are that network's
		}
		s := Server{Name: r.Name, Machine: r.ClientIdentifier, Token: r.AccessToken, Owned: r.Owned}
		seen := map[string]bool{}
		for _, cn := range r.Connections {
			if !cn.Local || cn.Relay {
				continue
			}
			ip := net.ParseIP(cn.Address)
			if ip == nil {
				ip = netguard.PlexDirectIP(cn.Address)
			}
			port := cn.Port
			if u, err := url.Parse(cn.URI); err == nil {
				if ip == nil {
					ip = netguard.PlexDirectIP(u.Hostname())
				}
				if port == 0 {
					port, _ = strconv.Atoi(u.Port())
				}
			}
			// Only an address on a home network: a listing that calls a public address local would
			// otherwise send the server's token there.
			if ip == nil || !netguard.Home(ip) || port <= 0 || port > 65535 {
				continue
			}
			base := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(port))
			if !seen[base] {
				seen[base] = true
				s.Bases = append(s.Bases, base)
			}
		}
		if len(s.Bases) == 0 {
			continue
		}
		if s.Owned {
			own = append(own, s)
		} else {
			shared = append(shared, s)
		}
	}
	return append(own, shared...), nil
}
