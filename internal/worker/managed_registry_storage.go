package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

type ManagedRegistryStorageRequest struct {
	Policy    ManagedRegistryRetention `json:"policy"`
	Protected []string                 `json:"protected"`
	Now       time.Time                `json:"now"`
	Delete    bool                     `json:"delete"`
}

type ManagedRegistryStorageResult struct {
	Manifests int `json:"manifests"`
	Deleted   int `json:"deleted"`
}

// This helper runs with the registry stopped and only its owned data volume mounted.
// Enumerating revisions (including untagged ones) avoids the catalog/tag API's gaps.
func RunManagedRegistryStorage(root string, input io.Reader, output io.Writer) error {
	var in ManagedRegistryStorageRequest
	d := json.NewDecoder(io.LimitReader(input, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("invalid registry maintenance request")
	}
	if err := in.Policy.Validate(); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	base := "docker/registry/v2"
	var manifests []ManagedRegistryManifest
	tags := map[string][]string{}
	tagTimes := map[string]time.Time{}
	files := 0
	err = fs.WalkDir(r.FS(), base+"/repositories", func(name string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) && name == base+"/repositories" {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		files++
		if files > 131072 {
			return errors.New("registry inventory exceeds file limit")
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return errors.New("registry inventory contains a symbolic link")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("registry inventory contains a special file")
		}
		rel := strings.TrimPrefix(name, base+"/repositories/")
		repo, suffix, ok := strings.Cut(rel, "/_manifests/")
		if !ok {
			return nil
		}
		if ValidateRegistryRepository(repo) != nil {
			return errors.New("invalid registry repository on disk")
		}
		parts := strings.Split(suffix, "/")
		if len(parts) == 4 && parts[0] == "revisions" && parts[1] == "sha256" && parts[3] == "link" {
			digest := "sha256:" + parts[2]
			if !managedManifestDigest.MatchString(digest) {
				return errors.New("invalid registry revision")
			}
			link, err := registryStorageRead(r, name, 128)
			if err != nil || string(link) != digest {
				return errors.New("registry revision link does not match its identity")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			manifests = append(manifests, ManagedRegistryManifest{Repository: repo, Digest: digest, CreatedAt: info.ModTime()})
			if len(manifests) > 8192 {
				return errors.New("registry inventory exceeds manifest limit")
			}
		}
		if len(parts) == 4 && parts[0] == "tags" && parts[2] == "current" && parts[3] == "link" {
			link, err := registryStorageRead(r, name, 128)
			if err != nil || !managedManifestDigest.Match(link) {
				return errors.New("invalid registry tag link")
			}
			tags[repo+"@"+string(link)] = append(tags[repo+"@"+string(link)], parts[1])
			info, err := entry.Info()
			if err != nil {
				return err
			}
			key := repo + "@" + string(link)
			if info.ModTime().After(tagTimes[key]) {
				tagTimes[key] = info.ModTime()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	metadataBytes := 0
	for i := range manifests {
		m := &manifests[i]
		key := m.Repository + "@" + m.Digest
		seen[key] = true
		m.Tags = tags[key]
		if tagTimes[key].After(m.CreatedAt) {
			m.CreatedAt = tagTimes[key]
		}
		data, err := registryStorageBlob(r, base, m.Digest)
		if err != nil {
			return err
		}
		metadataBytes += len(data)
		var manifest struct {
			SchemaVersion int `json:"schemaVersion"`
			Config        struct {
				Digest string `json:"digest"`
			} `json:"config"`
			Manifests []struct {
				Digest string `json:"digest"`
			} `json:"manifests"`
		}
		if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 2 {
			return errors.New("unsupported registry manifest; deletion refused")
		}
		if len(manifest.Manifests) > 0 {
			for _, child := range manifest.Manifests {
				m.Children = append(m.Children, child.Digest)
			}
		} else {
			data, err = registryStorageBlob(r, base, manifest.Config.Digest)
			if err != nil {
				return err
			}
			metadataBytes += len(data)
			if !json.Valid(data) {
				return errors.New("invalid registry image configuration")
			}
		}
		if metadataBytes > 64<<20 {
			return errors.New("registry inventory metadata exceeds 64 MiB")
		}
	}
	for key := range tags {
		if !seen[key] {
			return errors.New("tag references a missing registry revision; deletion refused")
		}
	}
	for _, ref := range in.Protected {
		if !seen[ref] {
			return errors.New("protected registry reference is missing; deletion refused")
		}
	}
	remove, err := PlanManagedRegistryRetention(in.Policy, manifests, in.Protected, in.Now)
	if err != nil {
		return err
	}
	if in.Delete {
		for _, directory := range []string{base + "/repositories", base + "/blobs/sha256"} {
			if err = r.MkdirAll(directory, 0755); err != nil {
				return err
			}
		}
		for _, m := range remove {
			for _, tag := range m.Tags {
				if !managedBuildTag.MatchString(tag) {
					return errors.New("foreign tag selected for deletion")
				}
				if err = r.RemoveAll(base + "/repositories/" + m.Repository + "/_manifests/tags/" + tag); err != nil {
					return err
				}
			}
			if err = r.RemoveAll(base + "/repositories/" + m.Repository + "/_manifests/revisions/sha256/" + strings.TrimPrefix(m.Digest, "sha256:")); err != nil {
				return err
			}
		}
	}
	return json.NewEncoder(output).Encode(ManagedRegistryStorageResult{Manifests: len(manifests), Deleted: len(remove)})
}

func registryStorageRead(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("registry metadata is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(data) > int(limit) {
		return nil, errors.New("registry metadata exceeds limit")
	}
	return data, err
}

func registryStorageBlob(root *os.Root, base, digest string) ([]byte, error) {
	if !managedManifestDigest.MatchString(digest) {
		return nil, errors.New("unsupported registry content digest")
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	data, err := registryStorageRead(root, path.Join(base, "blobs/sha256", hexDigest[:2], hexDigest, "data"), 2<<20)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hexDigest {
		return nil, errors.New("registry content digest mismatch; deletion refused")
	}
	return data, nil
}
