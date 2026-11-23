package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"
)

type ObjectBucket struct {
	CheckOnly  bool   `json:"check_only,omitempty"`
	Revoke     bool   `json:"revoke,omitempty"`
	Workload   string `json:"workload"`
	Bucket     string `json:"bucket"`
	AccessKey  string `json:"access_key"`
	SecretKey  string `json:"secret_key"`
	AdminToken string `json:"admin_token"`
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
var garageKey = regexp.MustCompile(`^GK[a-f0-9]{24}$`)

func (e *Engine) ProvisionObjectBucket(ctx context.Context, in ObjectBucket) error {
	if !workloadIdentifier.MatchString(in.Workload) || !databaseSecret.MatchString(in.AdminToken) || !in.CheckOnly && (!bucketName.MatchString(in.Bucket) || !garageKey.MatchString(in.AccessKey) || !databaseSecret.MatchString(in.SecretKey)) {
		return errors.New("invalid Garage bucket request")
	}
	container, err := e.databaseService(ctx, in.Workload, "garage")
	if err != nil {
		return err
	}
	target, err := e.tunnelTarget(ctx, container, 3903)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	call := func(method, path string, body any, out any) error {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, "http://"+target+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+in.AdminToken)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return errors.New("Garage administration API unavailable")
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return errors.New("Garage bucket administration refused the change")
		}
		if out != nil {
			return json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(out)
		}
		return nil
	}
	if in.CheckOnly {
		return call("GET", "/v2/GetClusterHealth", nil, nil)
	}
	if in.Revoke {
		return call("POST", "/v2/DeleteKey?id="+in.AccessKey, nil, nil)
	}
	// A local alias gives each scoped key its own bucket namespace, so retries cannot claim another key's bucket.
	if err = call("GET", "/v2/GetKeyInfo?id="+in.AccessKey, nil, nil); err != nil {
		if err = call("POST", "/v2/ImportKey", map[string]any{"accessKeyId": in.AccessKey, "secretAccessKey": in.SecretKey, "name": "Cove workload bucket"}, nil); err != nil {
			return err
		}
	}
	var info struct {
		ID   string `json:"id"`
		Keys []struct {
			ID string `json:"accessKeyId"`
		} `json:"keys"`
	}
	query := "/v2/GetBucketInfo?globalAlias=" + in.Bucket
	if err = call("GET", query, nil, &info); err != nil {
		if err = call("POST", "/v2/CreateBucket", map[string]any{"globalAlias": in.Bucket, "localAlias": map[string]any{"accessKeyId": in.AccessKey, "alias": in.Bucket, "allow": map[string]bool{"read": true, "write": true, "owner": false}}}, &info); err != nil {
			return err
		}
	}
	owned := false
	for _, key := range info.Keys {
		if key.ID == in.AccessKey {
			owned = true
		}
	}
	if !owned {
		return errors.New("existing bucket does not belong to this scoped key")
	}
	return call("POST", "/v2/AllowBucketKey", map[string]any{"bucketId": info.ID, "accessKeyId": in.AccessKey, "permissions": map[string]bool{"read": true, "write": true, "owner": false}}, nil)
}
