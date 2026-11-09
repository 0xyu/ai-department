package plan

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/schemas"
)

type schemaNode struct {
	Type       any                    `json:"type"`
	Properties map[string]*schemaNode `json:"properties"`
	Required   []string               `json:"required"`
	Items      *schemaNode            `json:"items"`
	Ref        string                 `json:"$ref"`
	Enum       []string               `json:"enum"`
	Defs       map[string]*schemaNode `json:"$defs"`
	Additional *bool                  `json:"additionalProperties"`
}

func jsonFields(v any) []string {
	var out []string
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		if tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out
}

func propNames(n *schemaNode) []string {
	var out []string
	for k := range n.Properties {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// plan.schema.json describes exactly the fields the structured loader reads.
func TestPlanSchemaMatchesLoader(t *testing.T) {
	var root schemaNode
	if err := json.Unmarshal(schemas.Plan, &root); err != nil {
		t.Fatalf("plan.schema.json is not valid JSON: %v", err)
	}
	resolve := func(n *schemaNode) *schemaNode {
		if n.Ref != "" {
			return root.Defs[strings.TrimPrefix(n.Ref, "#/$defs/")]
		}
		return n
	}
	phase := resolve(root.Properties["phases"].Items)
	checks := []struct {
		name string
		node *schemaNode
		typ  any
	}{
		{"document", &root, structDoc{}},
		{"requirement", root.Properties["requirements"].Items, structReq{}},
		{"traceability entry", root.Properties["traceability"].Items, structTrace{}},
		{"phase", phase, structPhase{}},
		{"scope", phase.Properties["scope"], structScope{}},
		{"acceptance criterion", phase.Properties["acceptance_criteria"].Items, structAC{}},
		{"gate", root.Defs["gate"], structGate{}},
	}
	for _, c := range checks {
		if got, want := propNames(c.node), jsonFields(c.typ); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema properties %v, loader fields %v", c.name, got, want)
		}
		if c.node.Additional == nil || *c.node.Additional {
			t.Errorf("%s: additionalProperties must be false", c.name)
		}
	}
	gates := phase.Properties["gates"]
	if got := propNames(gates); !reflect.DeepEqual(got, sortedCopy(GateTypes)) || !reflect.DeepEqual(sortedCopy(gates.Required), sortedCopy(GateTypes)) {
		t.Errorf("gates: properties %v required %v", got, gates.Required)
	}
	if got := root.Defs["gate"].Properties["scanners"].Items.Enum; !reflect.DeepEqual(got, config.SecurityScanners) {
		t.Errorf("scanner enum %v, config %v", got, config.SecurityScanners)
	}
	// The valid fixture has every required key the schema asks for.
	pkg := mustLoad(t, validYAML)
	if len(pkg.Diagnostics) > 0 {
		t.Fatal(dump(pkg.Diagnostics))
	}
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
