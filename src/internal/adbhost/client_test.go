package adbhost

import (
	"strings"
	"testing"
)

func TestParseGetprop(t *testing.T) {
	properties, err := parseGetprop(strings.NewReader(`
[ro.product.name]: [oriole]
[ro.product.model]: [Pixel 6]
[ro.product.device]: [oriole]
invalid line
`))
	if err != nil {
		t.Fatalf("parseGetprop() error = %v", err)
	}

	if properties["ro.product.name"] != "oriole" {
		t.Fatalf("ro.product.name = %q, want oriole", properties["ro.product.name"])
	}
	if properties["ro.product.model"] != "Pixel 6" {
		t.Fatalf("ro.product.model = %q, want Pixel 6", properties["ro.product.model"])
	}
	if properties["ro.product.device"] != "oriole" {
		t.Fatalf("ro.product.device = %q, want oriole", properties["ro.product.device"])
	}
}
