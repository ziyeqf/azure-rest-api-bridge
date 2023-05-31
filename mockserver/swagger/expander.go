package swagger

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-openapi/spec"
	"github.com/magodo/azure-rest-api-bridge/mockserver/swagger/refutil"
)

type Expander struct {
	// Operation ref
	ref  spec.Ref
	root *Property
}

// NewExpander create a expander for the schema referenced by the input json reference.
// The reference must be a normalized reference.
func NewExpander(ref spec.Ref) (*Expander, error) {
	psch, ownRef, visited, ok, err := refutil.RResolve(ref, nil, false)
	if err != nil {
		return nil, fmt.Errorf("recursively resolve schema: %v", err)
	}
	if !ok {
		return nil, fmt.Errorf("circular ref found when resolving schema: %s", &ref)
	}

	return &Expander{
		ref: ref,
		root: &Property{
			Schema:      psch,
			ref:         ownRef,
			addr:        RootAddr,
			visitedRefs: visited,
		},
	}, nil
}

// NewExpanderFromGet create a expander for the successful response schema of an operation referenced by the input json reference.
// The reference must be a normalized reference to the get operation.
func NewExpanderFromGet(ref spec.Ref) (*Expander, error) {
	if !ref.HasFullFilePath {
		return nil, fmt.Errorf("reference %s is not normalized", &ref)
	}
	tks := ref.GetPointer().DecodedTokens()
	if l := len(tks); l == 0 || tks[l-1] != "get" {
		return nil, fmt.Errorf("reference %s is not pointing to `get` operation", &ref)
	}

	piref := refutil.Parent(ref)
	pi, err := spec.ResolvePathItemWithBase(nil, piref, nil)
	if err != nil {
		return nil, fmt.Errorf("resolving path item ref %s: %v", &piref, err)
	}
	op := pi.Get
	if op == nil {
		return nil, fmt.Errorf("no `get` operation defined by path item %s", &piref)
	}
	if op.Responses == nil {
		return nil, fmt.Errorf("operation refed by %s has no responses defined", &ref)
	}
	// We only care about 200 for now, probably we should extend to support the others (e.g. when 200 is not defined).
	if _, ok := op.Responses.StatusCodeResponses[http.StatusOK]; !ok {
		return nil, fmt.Errorf("operation refed by %s has no 200 responses object defined", &ref)
	}

	// In case the response is a ref itself, follow it
	respref := refutil.Append(ref, "responses", "200")
	_, respref, _, ok, err := refutil.RResolveResponse(respref, nil, false)
	if err != nil {
		return nil, fmt.Errorf("recursively resolve response ref %s: %v", &respref, err)
	}
	if !ok {
		return nil, fmt.Errorf("circular ref found when resolving response ref %s", &respref)
	}

	return NewExpander(refutil.Append(respref, "schema"))
}

func (e *Expander) Root() *Property {
	return e.root
}

func (e *Expander) Expand() error {
	wl := []*Property{e.root}
	for {
		if len(wl) == 0 {
			break
		}
		nwl := []*Property{}
		for _, prop := range wl {
			if err := e.expandPropStep(prop); err != nil {
				return err
			}
			if prop.Element != nil {
				nwl = append(nwl, prop.Element)
			}
			for _, v := range prop.Children {
				nwl = append(nwl, v)
			}
			for _, v := range prop.Variant {
				nwl = append(nwl, v)
			}
		}
		wl = nwl
	}
	return nil
}

func (e *Expander) expandPropStep(prop *Property) error {
	if len(prop.Schema.Type) > 1 {
		return fmt.Errorf("%s: type of property type is an array (not supported yet)", prop.addr)
	}
	schema := prop.Schema
	t := "object"
	if len(schema.Type) == 1 {
		t = schema.Type[0]
	}
	switch t {
	case "array":
		return e.expandPropStepAsArray(prop)
	case "object":
		if schema.Discriminator == "" {
			if SchemaIsMap(schema) {
				return e.expandPropAsMap(prop)
			}
			return e.expandPropAsRegularObject(prop)
		}
		return e.expandPropAsPolymorphicObject(prop)
	}
	return nil
}

func (e *Expander) expandPropStepAsArray(prop *Property) error {
	schema := prop.Schema
	if !SchemaIsArray(schema) {
		return fmt.Errorf("%s: is not array", prop.addr)
	}
	addr := append(prop.addr, PropertyAddrStep{
		Type: PropertyAddrStepTypeIndex,
	})
	if schema.Items.Schema == nil {
		return fmt.Errorf("%s: items of property is not a single schema (not supported yet)", addr)
	}
	schema, ownRef, visited, ok, err := refutil.RResolve(refutil.Append(prop.ref, "items"), prop.visitedRefs, false)
	if err != nil {
		return fmt.Errorf("%s: recursively resolving items: %v", addr, err)
	}
	if !ok {
		return nil
	}
	prop.Element = &Property{
		Schema:      schema,
		ref:         ownRef,
		addr:        addr,
		visitedRefs: visited,
	}
	return nil
}

func (e *Expander) expandPropAsMap(prop *Property) error {
	schema := prop.Schema
	if !SchemaIsMap(schema) {
		return fmt.Errorf("%s: is not map", prop.addr)
	}
	addr := append(PropertyAddr{}, prop.addr...)
	addr = append(addr, PropertyAddrStep{
		Type: PropertyAddrStepTypeIndex,
	})
	if schema.AdditionalProperties.Schema == nil {
		return fmt.Errorf("%s: additionalProperties is not a single schema (not supported yet)", addr)
	}
	schema, ownRef, visited, ok, err := refutil.RResolve(refutil.Append(prop.ref, "additionalProperties"), prop.visitedRefs, false)
	if err != nil {
		return fmt.Errorf("%s: recursively resolving additionalProperties: %v", addr, err)
	}
	if !ok {
		return nil
	}
	prop.Element = &Property{
		Schema:      schema,
		ref:         ownRef,
		addr:        addr,
		visitedRefs: visited,
	}
	return nil
}

func (e *Expander) expandPropAsRegularObject(prop *Property) error {
	schema := prop.Schema

	if !SchemaIsObject(schema) {
		return fmt.Errorf("%s: is not object", prop.addr)
	}

	prop.Children = map[string]*Property{}

	// Expanding the regular properties
	for k := range schema.Properties {
		addr := append(PropertyAddr{}, prop.addr...)
		addr = append(addr, PropertyAddrStep{
			Type:  PropertyAddrStepTypeProp,
			Value: k,
		})
		schema, ownRef, visited, ok, err := refutil.RResolve(refutil.Append(prop.ref, "properties", k), prop.visitedRefs, false)
		if err != nil {
			return fmt.Errorf("%s: recursively resolving property %s: %v", addr, k, err)
		}
		if !ok {
			continue
		}
		prop.Children[k] = &Property{
			Schema:      schema,
			ref:         ownRef,
			addr:        addr,
			visitedRefs: visited,
		}
	}

	// Inheriting the allOf schemas
	for i := range schema.AllOf {
		schema, ownRef, visited, ok, err := refutil.RResolve(refutil.Append(prop.ref, "allOf", strconv.Itoa(i)), prop.visitedRefs, false)
		if err != nil {
			return fmt.Errorf("%s: recursively resolving %d-th allOf schema: %v", prop.addr, i, err)
		}
		if !ok {
			continue
		}
		tmpExp := Expander{
			ref: ownRef,
			root: &Property{
				Schema:      schema,
				ref:         ownRef,
				addr:        prop.addr,
				visitedRefs: visited,
			},
		}
		// The base schema of a variant schema is always a regular object.
		if err := tmpExp.expandPropAsRegularObject(tmpExp.root); err != nil {
			return fmt.Errorf("%s: expanding the %d-th (temporary) allOf schema: %v", prop.addr, i, err)
		}
		for k, v := range tmpExp.root.Children {
			prop.Children[k] = v
		}
	}

	return nil
}

func (e *Expander) expandPropAsPolymorphicObject(prop *Property) error {
	schema := prop.Schema
	if !SchemaIsObject(schema) {
		return fmt.Errorf("%s: is not object", prop.addr)
	}
	prop.Variant = map[string]*Property{}

	dsch, _, _, _, err := refutil.RResolve(refutil.Append(prop.ref, "properties", schema.Discriminator), prop.visitedRefs, false)
	if err != nil {
		return fmt.Errorf("%s: recursively resolving discriminator property's(%s) schema: %v", prop.addr, schema.Discriminator, err)
	}
	for _, name := range dsch.Enum {
		name := name.(string)
		addr := append(PropertyAddr{}, prop.addr...)
		addr = append(addr, PropertyAddrStep{
			Type:  PropertyAddrStepTypeVariant,
			Value: name,
		})
		visited := map[string]bool{}
		for k, v := range prop.visitedRefs {
			// Remove the owning ref of the base schema from visited set in order to allow the later allOf inheritance.
			if k == prop.ref.String() {
				continue
			}
			visited[k] = v
		}
		vref := spec.MustCreateRef(prop.ref.GetURL().Path + "#/definitions/" + name)
		psch, ownRef, visited, ok, err := refutil.RResolve(vref, visited, true)
		if err != nil {
			return fmt.Errorf("%s: recursively resolving variant schema (%s): %v", addr, name, err)
		}
		if !ok {
			continue
		}
		prop.Variant[name] = &Property{
			Schema:      psch,
			ref:         ownRef,
			addr:        addr,
			visitedRefs: visited,
		}
	}
	return nil
}

func schemaTypeIsObject(schema *spec.Schema) bool {
	return len(schema.Type) == 0 || len(schema.Type) == 1 && schema.Type[0] == "object"
}

func SchemaIsArray(schema *spec.Schema) bool {
	return len(schema.Type) == 1 && schema.Type[0] == "array"
}

func SchemaIsObject(schema *spec.Schema) bool {
	return schemaTypeIsObject(schema) && !SchemaIsMap(schema)
}

func SchemaIsMap(schema *spec.Schema) bool {
	return schemaTypeIsObject(schema) && len(schema.Properties) == 0 && schema.AdditionalProperties != nil
}
