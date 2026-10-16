package pulse_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

func TestYAMLInputMapKeyCollision(t *testing.T) {
	for _, collision := range []bool{true, false} {
		t.Run(fmt.Sprint("collision=", collision), func(t *testing.T) {
			key := pulse.NewKey[map[int]string]("mapping")
			reg := pulse.NewRegistry()
			pulse.MustRegisterKey(reg, key)
			reg.MustRegister("reader", func(rc *pulse.RunCtx) error {
				got, err := pulse.Get(rc, key)
				if err != nil {
					return err
				}
				if len(got) != 2 || got[1] != "secret-integer" || got[2] != "secret-float" {
					return fmt.Errorf("distinct map keys were not preserved: len=%d", len(got))
				}
				return nil
			})
			second := "2.0"
			if collision {
				second = "1.0"
			}
			doc := fmt.Sprintf(`version: 1
seeds:
  - key: {name: mapping, type: 'map[int]string'}
    from: {value: {1: secret-integer, %s: secret-float}}
nodes:
  - id: reader
    uses: reader
    requires: [{name: mapping, type: 'map[int]string'}]
`, second)
			g, plan, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "map-conversion"})
			if err != nil {
				t.Fatal(err)
			}
			in, err := plan.Inputs(nil)
			if collision {
				if err == nil {
					t.Fatal("YAML seed plan accepted colliding int/float map keys")
				}
				if !strings.Contains(err.Error(), "mapping") || !strings.Contains(err.Error(), "map key") || strings.Contains(err.Error(), "secret-") {
					t.Fatalf("collision error lost key context or leaked values: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Run(context.Background(), in); err != nil {
				t.Fatal(err)
			}
		})
	}
}
