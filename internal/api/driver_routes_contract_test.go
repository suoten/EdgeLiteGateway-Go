package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/constants"
	"edgelite/internal/drivers"
)

// The gateway exposes the driver inventory twice: GET /drivers (extras.go) and
// GET /drivers/list (extended_endpoints2.go), and cmd/edgelite registers both
// route sets on the same group. The UI consumes each shape separately, so the two
// handlers must never disagree about which protocols exist, and the routes must
// not depend on which register function Echo happened to see last.

func TestDriverInventoryEndpointsAgree(t *testing.T) {
	drivers.RegisterAll()

	c1, rec1 := setupWithAdmin(http.MethodGet, "/api/v1/drivers", "")
	if err := handleListDrivers(c1); err != nil {
		t.Fatalf("handleListDrivers: %v", err)
	}
	c2, rec2 := setupWithAdmin(http.MethodGet, "/api/v1/drivers/list", "")
	if err := handleGetDriverList(c2); err != nil {
		t.Fatalf("handleGetDriverList: %v", err)
	}

	a := driverNamesFromEnvelope(t, rec1.Body.String())
	b := driverNamesFromEnvelope(t, rec2.Body.String())
	if len(a) == 0 {
		t.Fatal("both driver endpoints reported an empty inventory")
	}
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("the two driver endpoints disagree: /drivers=%v /drivers/list=%v", a, b)
	}

	c3, rec3 := setupWithAdmin(http.MethodGet, "/api/v1/drivers/protocols", "")
	if err := handleListProtocols(c3); err != nil {
		t.Fatalf("handleListProtocols: %v", err)
	}
	var env struct {
		Data struct {
			Protocols []string `json:"protocols"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec3.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable protocol list: %v (%s)", err, rec3.Body.String())
	}
	// /drivers/protocols must cover the canonical driver inventory plus every
	// accepted alias: the Python edition's get_all_protocol_keys() included
	// aliases, and ProtoForge's integration client filters its protocol map
	// against this list, so a missing alias reads as "gateway cannot accept
	// this protocol" and the push is skipped client-side.
	protocols := make(map[string]bool, len(env.Data.Protocols))
	for _, p := range env.Data.Protocols {
		protocols[p] = true
	}
	for _, name := range a {
		if !protocols[name] {
			t.Errorf("/drivers/protocols is missing canonical driver %q", name)
		}
	}
	for alias := range constants.ProtocolAliases {
		if !protocols[alias] {
			t.Errorf("/drivers/protocols is missing accepted alias %q", alias)
		}
	}
	if len(env.Data.Protocols) != len(protocols) {
		t.Fatalf("/drivers/protocols contains duplicates: %v", env.Data.Protocols)
	}
	for _, p := range env.Data.Protocols {
		if constants.NormalizeProtocol(p) == "" {
			t.Errorf("/drivers/protocols advertises %q, which NormalizeProtocol rejects", p)
		}
	}
}

func driverNamesFromEnvelope(t *testing.T, body string) []string {
	t.Helper()
	var env struct {
		Data struct {
			Drivers []struct {
				Name string `json:"name"`
			} `json:"drivers"`
			Total int `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unparseable driver list %q: %v", body, err)
	}
	names := make([]string, 0, len(env.Data.Drivers))
	for _, d := range env.Data.Drivers {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	if env.Data.Total != len(names) {
		t.Fatalf("total says %d but %d drivers are listed", env.Data.Total, len(names))
	}
	return names
}

// TestDriverRouteRegistrationIsOrderIndependent registers the two driver route
// sets in both orders and compares the resulting route inventory.
func TestDriverRouteRegistrationIsOrderIndependent(t *testing.T) {
	inventory := func(first, second func(g *echo.Group)) []string {
		e := echo.New()
		g := e.Group("/api/v1/drivers")
		first(g)
		second(g)
		var out []string
		for _, r := range e.Routes() {
			out = append(out, r.Method+" "+r.Path+" "+r.Name)
		}
		sort.Strings(out)
		return out
	}

	a := inventory(RegisterDriverRoutes, RegisterDriverExtendedRoutes)
	b := inventory(RegisterDriverExtendedRoutes, RegisterDriverRoutes)
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("route inventory depends on registration order:\n%s\n---\n%s", a, b)
	}
	// Each method+path must be claimed exactly once. A duplicate only appears to
	// work because Echo lets one handler win.
	seen := map[string]int{}
	for _, r := range a {
		fields := strings.Fields(r)
		key := strings.Join(fields[:2], " ")
		seen[key]++
	}
	for key, n := range seen {
		if n > 1 {
			t.Errorf("%s is registered %d times across the two driver route sets", key, n)
		}
	}
	if len(a) == 0 {
		t.Fatal("no driver routes were registered")
	}
}
