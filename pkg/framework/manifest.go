package framework

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"text/template"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
)

// Objects the suite creates are written as YAML manifests and rendered here.
// A manifest is easier to read, and easier to check against what a person would
// have applied by hand, than the equivalent tree of Go structs.
//
//go:embed manifests/*.yaml
var manifests embed.FS

// q renders a value as a quoted YAML scalar. JSON string quoting is a subset of
// YAML double-quoted style, so this is safe for names, paths and label values
// alike, and it removes every question about when a value needs quoting.
var manifestFuncs = template.FuncMap{
	"q": func(v any) (string, error) {
		b, err := json.Marshal(fmt.Sprint(v))
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
}

// render fills a manifest template and decodes it into a typed object.
func render(name string, data any, into runtime.Object) error {
	raw, err := manifests.ReadFile("manifests/" + name)
	if err != nil {
		return fmt.Errorf("reading manifest %s: %w", name, err)
	}
	tmpl, err := template.New(name).Funcs(manifestFuncs).Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parsing manifest %s: %w", name, err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return fmt.Errorf("rendering manifest %s: %w", name, err)
	}
	// UniversalDeserializer accepts YAML, so the manifest stays in the form a
	// person would apply. A decode failure names the manifest and the rendered
	// text, because an indentation slip is otherwise painful to find.
	if err := runtime.DecodeInto(scheme.Codecs.UniversalDeserializer(), out.Bytes(), into); err != nil {
		return fmt.Errorf("decoding manifest %s: %w\n---\n%s", name, err, out.String())
	}
	return nil
}
