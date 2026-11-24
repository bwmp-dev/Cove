package worker

import (
	"encoding/json"
	"errors"
	"strconv"
)

type ingressDockerBinding struct {
	HostIP   string
	HostPort string
}

func bindIngressProxy(body map[string]json.RawMessage, settings IngressExposureSettings, ports, previous map[string]int, replacePublic, hadHTTP3 bool) error {
	if err := validateListenerPorts(settings, ports); err != nil {
		return err
	}
	var host map[string]json.RawMessage
	if json.Unmarshal(body["HostConfig"], &host) != nil || host == nil {
		return errors.New("private proxy host settings are unreadable")
	}
	bindings := map[string][]ingressDockerBinding{}
	if len(host["PortBindings"]) > 0 && json.Unmarshal(host["PortBindings"], &bindings) != nil {
		return errors.New("private proxy port bindings are unreadable")
	}
	if bindings == nil {
		bindings = map[string][]ingressDockerBinding{}
	}
	if settings.HTTP3 && !hadHTTP3 && len(bindings["443/udp"]) > 0 {
		return errors.New("an unmanaged UDP publication reserves the HTTPS HTTP/3 port")
	}
	for _, port := range previous {
		delete(bindings, strconv.Itoa(port)+"/tcp")
		delete(bindings, strconv.Itoa(port)+"/udp")
	}
	convert := func(items []IngressBinding) []ingressDockerBinding {
		result := make([]ingressDockerBinding, 0, len(items))
		for _, item := range items {
			result = append(result, ingressDockerBinding{HostIP: item.Address, HostPort: strconv.Itoa(item.Port)})
		}
		return result
	}
	if replacePublic {
		delete(bindings, "80/tcp")
		delete(bindings, "443/tcp")
		if len(settings.Public.HTTP) > 0 {
			bindings["80/tcp"] = convert(settings.Public.HTTP)
		}
		if len(settings.Public.HTTPS) > 0 {
			bindings["443/tcp"] = convert(settings.Public.HTTPS)
		}
	}
	if hadHTTP3 {
		delete(bindings, "443/udp")
	}
	if settings.HTTP3 {
		if len(bindings["443/tcp"]) == 0 {
			return errors.New("HTTP/3 requires an HTTPS host publication")
		}
		bindings["443/udp"] = append([]ingressDockerBinding{}, bindings["443/tcp"]...)
	}
	for _, listener := range settings.Listeners {
		protocol := "tcp"
		if listener.Protocol == "udp" {
			protocol = "udp"
		}
		key := strconv.Itoa(ports[listener.ID]) + "/" + protocol
		if len(bindings[key]) > 0 {
			return errors.New("a managed listener overlaps an unmanaged container publication")
		}
		bindings[key] = convert(listener.Bindings)
	}
	encoded, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	host["PortBindings"] = encoded
	body["HostConfig"], err = json.Marshal(host)
	if err != nil {
		return err
	}
	exposed := map[string]json.RawMessage{}
	if len(body["ExposedPorts"]) > 0 && json.Unmarshal(body["ExposedPorts"], &exposed) != nil {
		return errors.New("private proxy exposed ports are unreadable")
	}
	if exposed == nil {
		exposed = map[string]json.RawMessage{}
	}
	for _, port := range previous {
		delete(exposed, strconv.Itoa(port)+"/tcp")
		delete(exposed, strconv.Itoa(port)+"/udp")
	}
	for key := range bindings {
		exposed[key] = json.RawMessage(`{}`)
	}
	body["ExposedPorts"], err = json.Marshal(exposed)
	return err
}
