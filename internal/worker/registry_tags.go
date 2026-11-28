package worker

import (
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var registryImageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func ValidRegistryTag(tag string) bool { return registryImageTag.MatchString(tag) }

const registryTagLimit = 4096
const registryPageLimit = 16
const registryResponseLimit = 1024 * 1024

type registryTagClient struct{ client *http.Client }

func newRegistryTagClient(selected ...outbound.Config) registryTagClient {
	config := outbound.Environment()
	if len(selected) > 0 {
		config = selected[0]
	}
	return registryTagClient{client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) { return config.For(r.URL.Scheme, r.URL.Host) }, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxResponseHeaderBytes: 64 * 1024, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func registryLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func registryJSON(response *http.Response, target any) error {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("registry request refused")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, registryResponseLimit+1))
	if err != nil || len(data) > registryResponseLimit {
		return errors.New("registry response exceeds its limit or is unavailable")
	}
	if json.Unmarshal(data, target) != nil {
		return errors.New("registry returned invalid JSON")
	}
	return nil
}

func registryBearerChallenge(value string) (map[string]string, error) {
	scheme, parameters, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || len(value) > 8192 {
		return nil, errors.New("registry Bearer challenge unavailable")
	}
	result := map[string]string{}
	for len(strings.TrimSpace(parameters)) > 0 {
		parameters = strings.TrimSpace(parameters)
		key, rest, found := strings.Cut(parameters, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if !found || key == "" || result[key] != "" {
			return nil, errors.New("invalid registry challenge")
		}
		if strings.HasPrefix(rest, "\"") {
			end := 1
			for end < len(rest) {
				if rest[end] == '\\' {
					end += 2
					continue
				}
				if rest[end] == '"' {
					break
				}
				end++
			}
			if end >= len(rest) {
				return nil, errors.New("invalid quoted registry challenge")
			}
			decoded, err := strconv.Unquote(rest[:end+1])
			if err != nil {
				return nil, errors.New("invalid registry challenge")
			}
			result[key] = decoded
			parameters = strings.TrimSpace(rest[end+1:])
			if parameters != "" {
				if parameters[0] != ',' {
					return nil, errors.New("invalid registry challenge separator")
				}
				parameters = parameters[1:]
			}
		} else {
			part, tail, _ := strings.Cut(rest, ",")
			result[key] = strings.TrimSpace(part)
			parameters = tail
		}
	}
	return result, nil
}

func (c registryTagClient) token(ctx context.Context, origin *url.URL, repository, challenge string, credentials *Registry) (string, error) {
	parameters, err := registryBearerChallenge(challenge)
	if err != nil {
		return "", err
	}
	realm, err := url.Parse(parameters["realm"])
	if err != nil || realm.Host == "" || realm.User != nil || realm.Fragment != "" || (realm.Scheme != "https" && !(realm.Scheme == "http" && origin.Scheme == "http" && realm.Host == origin.Host && registryLoopback(realm.Hostname()))) {
		return "", errors.New("registry token realm must use verified HTTPS")
	}
	// A registry challenge must not redirect credentials or requests to an arbitrary service.
	sameOrigin := realm.Scheme == origin.Scheme && realm.Host == origin.Host
	dockerHub := origin.Scheme == "https" && origin.Host == "registry-1.docker.io" && realm.Scheme == "https" && realm.Host == "auth.docker.io"
	if !sameOrigin && !dockerHub {
		return "", errors.New("registry token realm must match the registry origin or Docker Hub authentication origin")
	}
	q := realm.Query()
	q.Set("service", parameters["service"])
	q.Set("scope", "repository:"+repository+":pull")
	realm.RawQuery = q.Encode()
	request, err := http.NewRequestWithContext(ctx, "GET", realm.String(), nil)
	if err != nil {
		return "", err
	}
	if credentials != nil {
		request.SetBasicAuth(credentials.Username, credentials.Password)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return "", errors.New("registry token request failed")
	}
	var result struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err = registryJSON(response, &result); err != nil {
		return "", err
	}
	if result.Token == "" {
		result.Token = result.AccessToken
	}
	if result.Token == "" || len(result.Token) > 16384 || strings.ContainsAny(result.Token, "\r\n\x00") {
		return "", errors.New("registry returned an invalid token")
	}
	return result.Token, nil
}

func (c registryTagClient) tags(ctx context.Context, repository string, credentials *Registry) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	defer c.client.CloseIdleConnections()
	canonical := imageRepository(repository)
	host, path, ok := strings.Cut(canonical, "/")
	if !ok || ValidateRegistryRepository(path) != nil {
		return nil, errors.New("invalid registry repository")
	}
	if credentials != nil {
		if err := ValidateRegistry(credentials); err != nil {
			return nil, err
		}
		if imageRepository(credentials.Server+"/"+path) != canonical {
			credentials = nil
		}
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	origin := &url.URL{Scheme: "https", Host: host, Path: "/v2/" + path + "/tags/list", RawQuery: "n=256"}
	if registryLoopback(origin.Hostname()) {
		origin.Scheme = "http"
	}
	next := *origin
	var tags []string
	visited := map[string]bool{}
	authorization := ""
	for page := 0; page < registryPageLimit; page++ {
		if visited[next.String()] {
			return nil, errors.New("registry pagination repeated a page")
		}
		visited[next.String()] = true
		var response *http.Response
		for attempt := 0; attempt < 2; attempt++ {
			request, err := http.NewRequestWithContext(ctx, "GET", next.String(), nil)
			if err != nil {
				return nil, err
			}
			if authorization != "" {
				request.Header.Set("Authorization", "Bearer "+authorization)
			}
			response, err = c.client.Do(request)
			if err != nil {
				return nil, errors.New("registry tag listing failed")
			}
			if response.StatusCode != 401 || attempt == 1 {
				break
			}
			challenge := response.Header.Get("WWW-Authenticate")
			response.Body.Close()
			if strings.HasPrefix(strings.ToLower(challenge), "basic ") && credentials != nil {
				request.SetBasicAuth(credentials.Username, credentials.Password)
				response, err = c.client.Do(request)
				if err != nil {
					return nil, errors.New("registry authentication failed")
				}
				break
			}
			authorization, err = c.token(ctx, origin, path, challenge, credentials)
			if err != nil {
				return nil, err
			}
		}
		link := response.Header.Get("Link")
		var data struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if err := registryJSON(response, &data); err != nil {
			return nil, err
		}
		if data.Name != path || len(data.Tags)+len(tags) > registryTagLimit {
			return nil, errors.New("registry tag list is mismatched or exceeds 4096 tags")
		}
		for _, tag := range data.Tags {
			if len(tag) > 128 || !ValidRegistryTag(tag) {
				return nil, errors.New("registry returned an invalid tag")
			}
		}
		tags = append(tags, data.Tags...)
		if link == "" {
			return tags, nil
		}
		if len(link) > 4096 || !strings.HasPrefix(link, "<") {
			return nil, errors.New("invalid registry pagination link")
		}
		end := strings.IndexByte(link, '>')
		if end < 1 || !strings.Contains(link[end+1:], `rel="next"`) {
			return nil, errors.New("invalid registry pagination relation")
		}
		target, err := url.Parse(link[1:end])
		if err != nil {
			return nil, errors.New("invalid registry pagination URL")
		}
		target = origin.ResolveReference(target)
		if target.Scheme != origin.Scheme || target.Host != origin.Host || target.User != nil || target.Fragment != "" || target.Path != origin.Path {
			return nil, errors.New("registry pagination cannot leave the repository")
		}
		next = *target
	}
	return nil, errors.New("registry tag listing exceeds 16 pages")
}
