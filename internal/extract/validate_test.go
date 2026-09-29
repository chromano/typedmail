package extract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func inboxValidator(t *testing.T, slug string) func(out string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../schemas/" + slug + ".json")
	if err != nil {
		t.Fatal(err)
	}
	sch, err := validator(raw)
	if err != nil {
		t.Fatal(err)
	}
	return func(out string) []string {
		t.Helper()
		problems, err := validate(sch, json.RawMessage(out))
		if err != nil {
			t.Fatal(err)
		}
		return problems
	}
}

func TestValidate(t *testing.T) {
	shipment := inboxValidator(t, "shipments")
	order := inboxValidator(t, "orders")

	tests := []struct {
		name  string
		check func(string) []string
		out   string
		want  []string // a substring of each problem, in order
	}{
		{"valid, with null for what the email doesn't state", shipment, `{
			"carrier": "UPS", "tracking_number": "1Z999", "tracking_url": null, "status": "shipped",
			"po_number": null, "shipped_at": null, "estimated_delivery": "2026-10-02",
			"exception_reason": null, "items": null}`, nil},
		{"formats are enforced", shipment, `{
			"carrier": "UPS", "tracking_number": "1Z999", "tracking_url": "ups dot com", "status": "shipped",
			"po_number": null, "shipped_at": "yesterday", "estimated_delivery": "2026-10-02",
			"exception_reason": null, "items": []}`,
			[]string{"/shipped_at: ", "/tracking_url: "}},
		{"a left-out field is reported", shipment, `{
			"carrier": "UPS", "tracking_number": "1Z999", "tracking_url": null, "status": "shipped",
			"po_number": null, "shipped_at": null, "estimated_delivery": null, "exception_reason": null}`,
			[]string{"/: missing property 'items'"}},
		{"pattern and minimum are enforced", order, `{
			"po_number": "10442", "buyer": {"name": null, "company": null, "email": "buyer@acme.com"},
			"items": [{"sku": null, "description": "Widget", "quantity": 0, "unit_price": "4.50"}],
			"total": "12", "currency": "usd", "deliver_by": null, "ship_to": null}`,
			[]string{"/currency: ", "/items/0/quantity: "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.check(tt.out)
			if len(got) != len(tt.want) {
				t.Fatalf("problems = %q, want %d", got, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}
