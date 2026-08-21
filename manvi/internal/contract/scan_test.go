package contract

import (
	"fmt"
	"testing"
)

func TestScanReport(t *testing.T) {
	m, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.FlagsWithoutReaders("flags/catalog.go", nil) {
		fmt.Println("FLAG   ", f.Name, f.Where)
	}
	for _, f := range m.FieldsWithoutReaders("Definition", "agents/definition.go", nil) {
		fmt.Println("FIELD  ", f.Name, f.Where)
	}
	for _, f := range m.ArgFieldsWithoutReaders(nil) {
		fmt.Println("ARG    ", f.Name, f.Where)
	}
}
