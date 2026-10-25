package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
)

// GitHubAPI is the public GitHub REST API.
const GitHubAPI = "https://api.github.com"

// listLimit bounds the releases list body.
const listLimit = 16 << 20

// GitHub reads the releases of one repository through the REST API. With a
// Token (needed while the repository is private, decision 0063 Q2) every
// API request carries it; without one no Authorization header is sent.
type GitHub struct {
	Owner, Repo string
	Token       string
	BaseURL     string        // GitHubAPI unless a test points it elsewhere
	Timeout     time.Duration // bounds Releases; Open is bounded by its context
	HTTP        *http.Client
}

// NewGitHub is a source for owner/repo.
func NewGitHub(owner, repo, token string) *GitHub {
	g := &GitHub{Owner: owner, Repo: repo, Token: token, BaseURL: GitHubAPI, Timeout: 20 * time.Second}
	g.HTTP = &http.Client{CheckRedirect: g.redirect}
	return g
}

// TokenFromEnv is GH_TOKEN, else GITHUB_TOKEN, as gh reads them.
func TokenFromEnv(getenv func(string) string) string {
	if t := getenv("GH_TOKEN"); t != "" {
		return t
	}
	return getenv("GITHUB_TOKEN")
}

func (g *GitHub) Name() string { return "github:" + g.Owner + "/" + g.Repo }

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Prerelease  bool      `json:"prerelease"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
	Body        string    `json:"body"`
	Assets      []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"url"`
	} `json:"assets"`
}

// Releases lists the latest 100 published releases (drafts, which a token
// with push access also sees, are left out).
func (g *GitHub) Releases(ctx context.Context) ([]Release, error) {
	if g.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.Timeout)
		defer cancel()
	}
	u := strings.TrimRight(g.BaseURL, "/") + "/repos/" + url.PathEscape(g.Owner) + "/" + url.PathEscape(g.Repo) + "/releases?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, g.statusError(resp)
	}
	var list []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, listLimit)).Decode(&list); err != nil {
		return nil, fmt.Errorf("%s: the releases list is not JSON: %w", g.Name(), err)
	}
	rels := make([]Release, 0, len(list))
	for _, r := range list {
		if r.Draft {
			continue
		}
		rel := Release{Version: strings.TrimPrefix(r.TagName, "v"), Tag: r.TagName, Prerelease: r.Prerelease, Published: r.PublishedAt, Notes: r.Body}
		for _, a := range r.Assets {
			rel.Assets = append(rel.Assets, Asset{Name: a.Name, Size: a.Size, URL: a.URL})
		}
		rels = append(rels, rel)
	}
	return rels, nil
}

// Open downloads an asset through its API URL with Accept:
// application/octet-stream, which works for private repositories too; the
// API redirects to a storage host, where the token is not sent.
func (g *GitHub) Open(ctx context.Context, rel Release, name string) (io.ReadCloser, error) {
	a, ok := rel.Asset(name)
	if !ok {
		return nil, fmt.Errorf("%s: release %s has no asset %s", g.Name(), rel.Tag, name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := g.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, fmt.Errorf("%s: %s: %w", g.Name(), name, g.statusError(resp))
	}
	return resp.Body, nil
}

// do sends req with the common headers; the token only goes to the API host.
func (g *GitHub) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "backsight/"+buildinfo.Version)
	if g.Token != "" && g.apiHost(req.URL) {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	c := g.HTTP
	if c == nil {
		c = &http.Client{CheckRedirect: g.redirect}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s unreachable: %w", g.Name(), err)
	}
	return resp, nil
}

// redirect drops the token on a redirect off the API host (Go keeps it
// for another port of the same host name).
func (g *GitHub) redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !g.apiHost(req.URL) {
		req.Header.Del("Authorization")
	}
	return nil
}

func (g *GitHub) apiHost(u *url.URL) bool {
	base, err := url.Parse(g.BaseURL)
	return err == nil && strings.EqualFold(base.Host, u.Host) && base.Scheme == u.Scheme
}

func (g *GitHub) statusError(resp *http.Response) error {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	msg := fmt.Sprintf("%s answered %d", g.Name(), resp.StatusCode)
	if body.Message != "" {
		msg += " (" + body.Message + ")"
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && g.Token == "":
		msg += ": a private repository needs a token in GH_TOKEN or GITHUB_TOKEN"
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusUnauthorized:
		msg += ": check that the token in GH_TOKEN or GITHUB_TOKEN can read the repository"
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		msg += ": rate limited or forbidden; a token in GH_TOKEN raises the limit"
	}
	return errors.New(msg)
}
