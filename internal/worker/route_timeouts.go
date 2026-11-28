package worker

import (
	"errors"
	"strconv"
)

type RouteTimeouts struct {
	DialSeconds           int `json:"dial_seconds,omitempty"`
	ResponseHeaderSeconds int `json:"response_header_seconds,omitempty"`
	IdleConnectionSeconds int `json:"idle_connection_seconds,omitempty"`
}

type ingressServersTransport struct {
	ForwardingTimeouts map[string]string `json:"forwardingTimeouts"`
}

func validateRouteTimeouts(value *RouteTimeouts) error {
	if value == nil {
		return nil
	}
	if value.DialSeconds < 0 || value.DialSeconds > 600 || value.ResponseHeaderSeconds < 0 || value.ResponseHeaderSeconds > 600 || value.IdleConnectionSeconds < 0 || value.IdleConnectionSeconds > 3600 {
		return errors.New("upstream dial and response-header timeouts must be zero to 600 seconds; idle connection timeout zero to 3600 seconds")
	}
	if *value == (RouteTimeouts{}) {
		return errors.New("omit empty upstream timeouts to keep the proxy defaults")
	}
	return nil
}

func (c *dynamicIngress) addRouteTimeouts(name string, value *RouteTimeouts, service *ingressService) {
	if value == nil {
		return
	}
	timeouts := map[string]string{}
	for _, entry := range []struct {
		name    string
		seconds int
	}{{"dialTimeout", value.DialSeconds}, {"responseHeaderTimeout", value.ResponseHeaderSeconds}, {"idleConnTimeout", value.IdleConnectionSeconds}} {
		if entry.seconds > 0 {
			timeouts[entry.name] = strconv.Itoa(entry.seconds) + "s"
		}
	}
	if c.HTTP.ServersTransports == nil {
		c.HTTP.ServersTransports = map[string]ingressServersTransport{}
	}
	c.HTTP.ServersTransports[name] = ingressServersTransport{ForwardingTimeouts: timeouts}
	service.LoadBalancer.ServersTransport = name
}
