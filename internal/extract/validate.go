package extract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	textmessage "golang.org/x/text/message"
)

// validator compiles the complete inbox schema (see completeSchema) with
// every constraint the wire schema only describes, such as pattern, minimum
// and format, so that validation enforces them.
func validator(schema json.RawMessage) (*jsonschema.Schema, error) {
	m, err := completeSchema(schema)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	// The validator needs numbers as json.Number, which its own decoder gives.
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("inbox.json", doc); err != nil {
		return nil, fmt.Errorf("compile inbox schema: %w", err)
	}
	sch, err := c.Compile("inbox.json")
	if err != nil {
		return nil, fmt.Errorf("compile inbox schema: %w", err)
	}
	return sch, nil
}

var printer = textmessage.NewPrinter(language.English)

// validate checks out against sch and returns one message per violation,
// sorted, such as "/items/0/quantity: minimum: got 0, want 1". It returns
// none when out matches.
func validate(sch *jsonschema.Schema, out json.RawMessage) ([]string, error) {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(out))
	if err != nil {
		return nil, err
	}
	err = sch.Validate(v)
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil, err
	}
	var problems []string
	var leaves func(e *jsonschema.ValidationError)
	leaves = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			problems = append(problems,
				"/"+strings.Join(e.InstanceLocation, "/")+": "+e.ErrorKind.LocalizedString(printer))
			return
		}
		for _, c := range e.Causes {
			leaves(c)
		}
	}
	leaves(ve)
	slices.Sort(problems)
	return problems, nil
}
