package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

type recipeLayer struct {
	Image   string   `json:"image,omitempty"`
	Step    string   `json:"step,omitempty"`
	Local   bool     `json:"local,omitempty"`
	Spread  bool     `json:"spread,omitempty"`
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

type recipeStep struct {
	Name      string            `json:"name"`
	Inputs    []recipeLayer     `json:"inputs,omitempty"`
	Commands  []json.RawMessage `json:"commands,omitempty"`
	Secrets   []string          `json:"secrets,omitempty"`
	Assets    map[string]string `json:"assets,omitempty"`
	Variables map[string]string `json:"variables,omitempty"`
	Caches    []string          `json:"caches,omitempty"`
}

type recipePlan struct {
	Schema  string       `json:"$schema,omitempty"`
	Steps   []recipeStep `json:"steps"`
	Secrets []string     `json:"secrets,omitempty"`
	Caches  map[string]struct {
		Directory string `json:"directory"`
		Type      string `json:"type,omitempty"`
	} `json:"caches,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
	Deploy  struct {
		Base         recipeLayer       `json:"base"`
		Inputs       []recipeLayer     `json:"inputs,omitempty"`
		StartCommand string            `json:"startCommand,omitempty"`
		Variables    map[string]string `json:"variables,omitempty"`
		Paths        []string          `json:"paths,omitempty"`
	} `json:"deploy"`
}

func decodeRecipeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("Railpack output has an unsupported or invalid field")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("Railpack output contains trailing data")
	}
	return nil
}

func pinRecipeImage(image string) (string, error) {
	switch image {
	case "", RailpackBuilderImage, RailpackRuntimeImage:
		return image, nil
	case "ghcr.io/railwayapp/railpack-builder:mise-" + RailpackMiseVersion:
		return RailpackBuilderImage, nil
	case "ghcr.io/railwayapp/railpack-runtime:mise-" + RailpackMiseVersion:
		return RailpackRuntimeImage, nil
	default:
		return "", errors.New("Railpack cannot replace Cove's pinned builder or runtime images")
	}
}

func recipeSecretNames(r Request) []string {
	var names []string
	for _, secret := range r.FileSecrets {
		for _, service := range secret.BuildServices {
			if service == r.BuildRecipe.Service {
				names = append(names, secret.Name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

func restrictRecipeSecrets(requested, selected []string) ([]string, error) {
	allowed := make(map[string]bool, len(selected))
	for _, name := range selected {
		allowed[name] = true
	}
	result := map[string]bool{}
	for _, name := range requested {
		if name == "*" {
			for name := range allowed {
				result[name] = true
			}
		} else if allowed[name] {
			result[name] = true
		} else {
			return nil, errors.New("Railpack requested an unselected build secret")
		}
	}
	names := make([]string, 0, len(result))
	for name := range result {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func validateRecipeCommand(raw json.RawMessage) (json.RawMessage, error) {
	var command map[string]json.RawMessage
	if json.Unmarshal(raw, &command) != nil {
		return nil, errors.New("Railpack commands must use explicit object fields")
	}
	var value any
	switch {
	case command["cmd"] != nil:
		value = &struct {
			Cmd        string `json:"cmd"`
			CustomName string `json:"customName,omitempty"`
		}{}
	case command["src"] != nil:
		copy := &struct {
			Src   string `json:"src"`
			Dest  string `json:"dest"`
			Image string `json:"image,omitempty"`
		}{}
		if err := decodeRecipeJSON(raw, copy); err != nil {
			return nil, err
		}
		var err error
		copy.Image, err = pinRecipeImage(copy.Image)
		if err != nil {
			return nil, err
		}
		return json.Marshal(copy)
	case command["path"] != nil && command["name"] != nil:
		value = &struct {
			Path       string `json:"path"`
			Name       string `json:"name"`
			Mode       uint32 `json:"mode,omitempty"`
			CustomName string `json:"customName,omitempty"`
		}{}
	case command["path"] != nil:
		value = &struct {
			Path string `json:"path"`
		}{}
	default:
		return nil, errors.New("Railpack emitted an unsupported command")
	}
	if err := decodeRecipeJSON(raw, value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func validateRecipePlan(data []byte, selectedSecrets []string) ([]byte, error) {
	if len(data) == 0 || len(data) > 2<<20 {
		return nil, errors.New("Railpack plan exceeds the 2 MiB limit or is empty")
	}
	var plan recipePlan
	if err := decodeRecipeJSON(data, &plan); err != nil {
		return nil, err
	}
	if len(plan.Steps) == 0 || len(plan.Steps) > 128 || len(plan.Caches) > 128 || len(plan.Deploy.Inputs) > 128 {
		return nil, errors.New("Railpack plan has too many steps or caches")
	}
	names := map[string]int{}
	for i, step := range plan.Steps {
		if step.Name == "" || len(step.Name) > 200 || strings.ContainsAny(step.Name, "\x00\r\n") {
			return nil, errors.New("Railpack step name is invalid")
		}
		if _, exists := names[step.Name]; exists {
			return nil, errors.New("Railpack step names must be unique")
		}
		names[step.Name] = i
	}
	checkLayer := func(layer *recipeLayer) error {
		var err error
		layer.Image, err = pinRecipeImage(layer.Image)
		if err != nil {
			return err
		}
		choices := 0
		if layer.Image != "" {
			choices++
		}
		if layer.Step != "" {
			choices++
		}
		if layer.Local {
			choices++
		}
		if layer.Spread {
			choices++
		}
		if choices != 1 {
			return errors.New("Railpack input must select exactly one contained source")
		}
		if layer.Step != "" {
			if _, exists := names[layer.Step]; !exists {
				return errors.New("Railpack input refers to an unknown step")
			}
		}
		return nil
	}
	var err error
	plan.Secrets, err = restrictRecipeSecrets(plan.Secrets, selectedSecrets)
	if err != nil {
		return nil, err
	}
	// Empty planner values deliberately carry no credentials; explicitly declare the selected mount IDs for the frontend.
	plan.Secrets = append([]string{}, selectedSecrets...)
	for i := range plan.Steps {
		step := &plan.Steps[i]
		if len(step.Inputs) > 128 || len(step.Commands) > 2048 {
			return nil, errors.New("Railpack step exceeds input or command limits")
		}
		for j := range step.Inputs {
			if err := checkLayer(&step.Inputs[j]); err != nil {
				return nil, err
			}
		}
		step.Secrets, err = restrictRecipeSecrets(step.Secrets, selectedSecrets)
		if err != nil {
			return nil, err
		}
		for j, command := range step.Commands {
			step.Commands[j], err = validateRecipeCommand(command)
			if err != nil {
				return nil, err
			}
		}
		for _, name := range step.Caches {
			if _, exists := plan.Caches[name]; !exists {
				return nil, errors.New("Railpack refers to an unknown cache")
			}
		}
	}
	for _, cache := range plan.Caches {
		if cache.Directory == "" || cache.Type != "" && cache.Type != "shared" && cache.Type != "locked" {
			return nil, errors.New("Railpack cache is invalid")
		}
	}
	if err := checkLayer(&plan.Deploy.Base); err != nil {
		return nil, err
	}
	for i := range plan.Deploy.Inputs {
		if err := checkLayer(&plan.Deploy.Inputs[i]); err != nil {
			return nil, err
		}
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] {
			return errors.New("Railpack plan has a dependency cycle")
		}
		if done[name] {
			return nil
		}
		visiting[name] = true
		for _, input := range plan.Steps[names[name]].Inputs {
			if input.Step != "" {
				if err := visit(input.Step); err != nil {
					return err
				}
			}
		}
		visiting[name], done[name] = false, true
		return nil
	}
	for name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return json.Marshal(plan)
}

func invalidateRecipeCommands(data []byte, nonce string) ([]byte, error) {
	var plan recipePlan
	if err := decodeRecipeJSON(data, &plan); err != nil {
		return nil, err
	}
	for i := range plan.Steps {
		for j, raw := range plan.Steps[i].Commands {
			var command struct {
				Cmd        string `json:"cmd"`
				CustomName string `json:"customName,omitempty"`
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil {
				return nil, errors.New("invalid validated command")
			}
			if fields["cmd"] == nil {
				continue
			}
			if err := json.Unmarshal(raw, &command); err != nil {
				return nil, err
			}
			if command.Cmd == "..." || command.Cmd == "sh -c '...'" {
				continue
			}
			// The pinned frontend does not consume Buildx's no-cache flag. A public operation nonce changes each exec identity without storing secret hashes or runtime environment values.
			command.Cmd = "sh -c ': " + nonce + "; exec \"$@\"' -- " + command.Cmd
			encoded, err := json.Marshal(command)
			if err != nil {
				return nil, err
			}
			plan.Steps[i].Commands[j] = encoded
		}
	}
	return json.Marshal(plan)
}
