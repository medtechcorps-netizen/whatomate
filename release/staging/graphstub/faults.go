package graphstub

import (
	"errors"
	"net/http"
	"regexp"
	"time"
)

const (
	maxQueuedFaults = 100
	maxFaultDelay   = 30 * time.Second
	routeAny        = "any"
)

var faultMessagePattern = regexp.MustCompile(`^[\x20-\x7e]{1,256}$`)

var faultRoutes = map[string]bool{
	routeAny: true, routeOAuth: true, routeDebugToken: true, routePhone: true, routeWABA: true,
	routeMedia: true, routeTemplate: true, routeMessages: true, routeMediaUpload: true,
	routeMediaDownload: true, routePhoneNumbers: true, routeTemplates: true,
	routeSubscribedApps: true, routeRegister: true, routeBusinessProfile: true,
}

// Fault makes the next Times requests on Route fail. Status answers with a
// Graph error envelope; Drop closes the connection without a response, so
// the caller sees an ambiguous transport failure; DelayMS stalls the request
// first and, alone, lets it then succeed.
type Fault struct {
	Route     string `json:"route"`
	Status    int    `json:"status,omitempty"`
	Code      int    `json:"code,omitempty"`
	Subcode   int    `json:"error_subcode,omitempty"`
	Message   string `json:"message,omitempty"`
	Transient bool   `json:"transient,omitempty"`
	Drop      bool   `json:"drop,omitempty"`
	DelayMS   int    `json:"delay_ms,omitempty"`
	Times     int    `json:"times,omitempty"`
}

func (f *Fault) normalize() error {
	if !faultRoutes[f.Route] {
		return errors.New("route is not a Graph route name")
	}
	if f.Times == 0 {
		f.Times = 1
	}
	switch {
	case f.Times < 1 || f.Times > 1000:
		return errors.New("times must be 1 to 1000")
	case f.Status != 0 && (f.Status < 400 || f.Status > 599):
		return errors.New("status must be 400 to 599")
	case f.Status != 0 && f.Drop:
		return errors.New("status and drop are exclusive")
	case f.DelayMS < 0 || time.Duration(f.DelayMS)*time.Millisecond > maxFaultDelay:
		return errors.New("delay_ms must be 0 to 30000")
	case f.Status == 0 && !f.Drop && f.DelayMS == 0:
		return errors.New("a fault needs status, drop or delay_ms")
	case f.Message != "" && !faultMessagePattern.MatchString(f.Message):
		return errors.New("message must be at most 256 printable characters")
	}
	if f.Status != 0 && f.Code == 0 {
		f.Code = 100
		if f.Status >= 500 {
			f.Code = 1
		}
	}
	if f.Status != 0 && f.Message == "" {
		f.Message = "Synthetic fault from the Graph stub"
	}
	return nil
}

// applyFault consumes the first queued fault for the request's route. It
// reports whether the fault answered the request.
func (s *Server) applyFault(c *call) bool {
	s.mu.Lock()
	var fault Fault
	found := false
	for index, queued := range s.faults {
		if queued.Route == c.entry.Route || queued.Route == routeAny {
			fault, found = *queued, true
			queued.Times--
			if queued.Times <= 0 {
				s.faults = append(s.faults[:index], s.faults[index+1:]...)
			}
			break
		}
	}
	s.mu.Unlock()
	if !found {
		return false
	}
	c.entry.Fault = true
	if fault.DelayMS > 0 {
		timer := time.NewTimer(time.Duration(fault.DelayMS) * time.Millisecond)
		select {
		case <-timer.C:
		case <-c.r.Context().Done():
			timer.Stop()
		}
	}
	switch {
	case fault.Drop:
		c.entry.Error = "fault_drop"
		if conn, _, err := c.w.Hijack(); err == nil {
			_ = conn.Close()
		} else {
			// HTTP/2 and test recorders cannot hijack; abort the handler instead.
			panic(http.ErrAbortHandler)
		}
		return true
	case fault.Status != 0:
		c.entry.Error = "fault"
		graphError(c, fault.Status, fault.Code, fault.Subcode, "OAuthException", fault.Message, fault.Transient)
		return true
	}
	return false
}
