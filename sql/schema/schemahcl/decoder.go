package schemahcl

import (
	"fmt"
	"sort"
	"strconv"

	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/schema/schemaspec"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

// Decode implements schema.Decoder. It parses an HCL document describing a schema into Spec.
func Decode(body []byte, spec schemaspec.Spec) error {
	parser := hclparse.NewParser()
	srcHCL, diag := parser.ParseHCL(body, "in-memory.hcl")
	if diag.HasErrors() {
		return diag
	}
	if srcHCL == nil {
		return fmt.Errorf("schemahcl: contents is nil")
	}
	ctx, err := evalContext(srcHCL)
	if err != nil {
		return err
	}
	if s, ok := spec.(*schemaspec.Schema); ok {
		f := &schemaFile{}
		if diag := gohcl.DecodeBody(srcHCL.Body, ctx, f); diag.HasErrors() {
			return diag
		}
		if len(f.Schemas) > 0 {
			s.Name = f.Schemas[0].Name
		}
		for _, tbl := range f.Tables {
			spec, err := tbl.spec(ctx)
			if err != nil {
				return err
			}
			s.Tables = append(s.Tables, spec)
		}
		return nil
	}
	return fmt.Errorf("schemahcl: unsupported spec type %T", spec)
}

type (
	schemaFile struct {
		Schemas []struct {
			Name string `hcl:",label"`
		} `hcl:"schema,block"`
		Tables []*table `hcl:"table,block"`
		Remain hcl.Body `hcl:",remain"`
	}
	table struct {
		Name        string        `hcl:",label"`
		Schema      *schemaRef    `hcl:"schema,optional"`
		Columns     []*column     `hcl:"column,block"`
		PrimaryKey  *primaryKey   `hcl:"primary_key,block"`
		ForeignKeys []*foreignKey `hcl:"foreign_key,block"`
		Indexes     []*index      `hcl:"index,block"`
		Remain      hcl.Body      `hcl:",remain"`
	}
	column struct {
		Name      string      `hcl:",label"`
		TypeName  string      `hcl:"type"`
		Null      bool        `hcl:"null,optional"`
		Default   cty.Value   `hcl:"default,optional"`
		Overrides []*override `hcl:"dialect,block"`
		Remain    hcl.Body    `hcl:",remain"`
	}
	primaryKey struct {
		Columns []*columnRef `hcl:"columns,optional"`
		Remain  hcl.Body     `hcl:",remain"`
	}
	foreignKey struct {
		Symbol     string      `hcl:",label"`
		Columns    []columnRef `hcl:"columns"`
		RefColumns []columnRef `hcl:"references"`
		OnUpdate   string      `hcl:"on_update,optional"`
		OnDelete   string      `hcl:"on_delete,optional"`
		Remain     hcl.Body    `hcl:",remain"`
	}
	index struct {
		Name    string      `hcl:",label"`
		Columns []columnRef `hcl:"columns"`
		Unique  bool        `hcl:"unique"`
		Remain  hcl.Body    `hcl:",remain"`
	}
	schemaRef struct {
		Name string `cty:"name"`
	}
	columnRef struct {
		Name  string `cty:"name"`
		Table string `cty:"table"`
	}
	override struct {
		Dialect string   `hcl:",label"`
		Remain  hcl.Body `hcl:",remain"`
	}
)

func (t *table) spec(ctx *hcl.EvalContext) (*schemaspec.Table, error) {
	out := &schemaspec.Table{
		Name: t.Name,
	}
	if t.Schema != nil {
		out.SchemaName = t.Schema.Name
	}
	for _, col := range t.Columns {
		cs, err := col.spec(ctx)
		if err != nil {
			return nil, err
		}
		out.Columns = append(out.Columns, cs)
	}
	if t.PrimaryKey != nil {
		pk, err := t.PrimaryKey.spec(ctx)
		if err != nil {
			return nil, err
		}
		out.PrimaryKey = pk
	}
	for _, fk := range t.ForeignKeys {
		fks, err := fk.spec(ctx)
		if err != nil {
			return nil, err
		}
		out.ForeignKeys = append(out.ForeignKeys, fks)
	}
	for _, idx := range t.Indexes {
		is, err := idx.spec(ctx)
		if err != nil {
			return nil, err
		}
		out.Indexes = append(out.Indexes, is)
	}
	body, ok := t.Remain.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("schemahcl: expected remainder to be of type *hclsyntax.Body")
	}
	attrs, err := toAttrs(ctx, body.Attributes, skip("schema"))
	if err != nil {
		return nil, err
	}
	out.Attrs = attrs
	return out, nil
}

func (c *column) spec(ctx *hcl.EvalContext) (*schemaspec.Column, error) {
	spec := &schemaspec.Column{
		Name: c.Name,
		Type: c.TypeName,
		Null: c.Null,
	}
	if c.Default != cty.NilVal {
		v := &schemaspec.LiteralValue{}
		switch c.Default.Type() {
		case cty.String:
			v.V = strconv.Quote(c.Default.AsString())
		case cty.Number:
			v.V = fmt.Sprint(c.Default.AsBigFloat())
		case cty.Bool:
			v.V = strconv.FormatBool(c.Default.True())
		}
		spec.Default = v
	}
	for _, ov := range c.Overrides {
		ovspec, err := ov.spec(ctx)
		if err != nil {
			return nil, err
		}
		spec.Overrides = append(spec.Overrides, ovspec)
	}
	common, err := extractCommon(ctx, c.Remain, skip("type", "default", "null", "dialect"))
	if err != nil {
		return nil, err
	}
	spec.Attrs = common.attrs
	spec.Children = common.children
	return spec, nil
}

func (p *primaryKey) spec(ctx *hcl.EvalContext) (*schemaspec.PrimaryKey, error) {
	common, err := extractCommon(ctx, p.Remain, skip("columns"))
	if err != nil {
		return nil, err
	}
	pk := &schemaspec.PrimaryKey{
		Attrs:    common.attrs,
		Children: common.children,
	}
	for _, col := range p.Columns {
		pk.Columns = append(pk.Columns, &schemaspec.ColumnRef{Table: col.Table, Name: col.Name})
	}
	return pk, nil
}

func (p *foreignKey) spec(ctx *hcl.EvalContext) (*schemaspec.ForeignKey, error) {
	common, err := extractCommon(ctx, p.Remain, skip("columns", "references", "on_update", "on_delete"))
	if err != nil {
		return nil, err
	}
	fk := &schemaspec.ForeignKey{
		Symbol:   p.Symbol,
		Attrs:    common.attrs,
		Children: common.children,
		OnDelete: p.OnDelete,
		OnUpdate: p.OnUpdate,
	}
	for _, col := range p.Columns {
		fk.Columns = append(fk.Columns, &schemaspec.ColumnRef{Table: col.Table, Name: col.Name})
	}
	var refTable string
	for _, refCol := range p.RefColumns {
		if refTable != "" && refCol.Table != refTable {
			return nil, fmt.Errorf("schemahcl: expected all ref columns to be of same table for key %q", p.Symbol)
		}
		fk.RefColumns = append(fk.RefColumns, &schemaspec.ColumnRef{Table: refCol.Table, Name: refCol.Name})
	}
	return fk, nil
}

func (i *index) spec(ctx *hcl.EvalContext) (*schemaspec.Index, error) {
	common, err := extractCommon(ctx, i.Remain, skip("columns", "unique"))
	if err != nil {
		return nil, err
	}
	idx := &schemaspec.Index{
		Name:     i.Name,
		Unique:   i.Unique,
		Attrs:    common.attrs,
		Children: common.children,
	}
	for _, col := range i.Columns {
		idx.Columns = append(idx.Columns, &schemaspec.ColumnRef{Table: col.Table, Name: col.Name})
	}
	return idx, nil
}

func (o *override) spec(ctx *hcl.EvalContext) (*schemaspec.Override, error) {
	common, err := extractCommon(ctx, o.Remain, nil)
	if err != nil {
		return nil, err
	}
	return &schemaspec.Override{
		Dialect: o.Dialect,
		Resource: schemaspec.Resource{
			Attrs:    common.attrs,
			Children: common.children,
		},
	}, nil
}

func skip(lst ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(lst))
	for _, item := range lst {
		out[item] = struct{}{}
	}
	return out
}

func toAttrs(ctx *hcl.EvalContext, hclAttrs hclsyntax.Attributes, skip map[string]struct{}) ([]*schemaspec.Attr, error) {
	var attrs []*schemaspec.Attr
	for _, hclAttr := range hclAttrs {
		if shouldSkip(skip, hclAttr.Name) {
			continue
		}
		at := &schemaspec.Attr{K: hclAttr.Name}
		value, diag := hclAttr.Expr.Value(ctx)
		if diag.HasErrors() {
			return nil, diag
		}
		var err error
		if value.CanIterateElements() {
			at.V, err = extractListValue(value)
		} else {
			at.V, err = extractLiteralValue(value)
		}
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, at)
	}
	// hclsyntax.Attributes is an alias for map[string]*Attribute
	sort.Slice(attrs, func(i, j int) bool {
		return attrs[i].K < attrs[j].K
	})
	return attrs, nil
}

func shouldSkip(skip map[string]struct{}, key string) bool {
	if skip == nil {
		return false
	}
	_, exists := skip[key]
	return exists
}

func extractListValue(value cty.Value) (*schemaspec.ListValue, error) {
	lst := &schemaspec.ListValue{}
	it := value.ElementIterator()
	for it.Next() {
		_, v := it.Element()
		litv, err := extractLiteralValue(v)
		if err != nil {
			return nil, err
		}
		lst.V = append(lst.V, litv.V)
	}
	return lst, nil
}

func extractLiteralValue(value cty.Value) (*schemaspec.LiteralValue, error) {
	switch value.Type() {
	case cty.String:
		return &schemaspec.LiteralValue{V: strconv.Quote(value.AsString())}, nil
	case cty.Number:
		bf := value.AsBigFloat()
		num, _ := bf.Float64()
		return &schemaspec.LiteralValue{V: strconv.FormatFloat(num, 'f', -1, 64)}, nil
	case cty.Bool:
		return &schemaspec.LiteralValue{V: strconv.FormatBool(value.True())}, nil
	default:
		return nil, fmt.Errorf("schemahcl: unsupported type %q", value.Type().GoString())
	}
}

func toResource(ctx *hcl.EvalContext, block *hclsyntax.Block) (*schemaspec.Resource, error) {
	spec := &schemaspec.Resource{
		Type: block.Type,
	}
	if len(block.Labels) > 0 {
		spec.Name = block.Labels[0]
	}
	attrs, err := toAttrs(ctx, block.Body.Attributes, nil)
	if err != nil {
		return nil, err
	}
	spec.Attrs = attrs
	for _, blk := range block.Body.Blocks {
		res, err := toResource(ctx, blk)
		if err != nil {
			return nil, err
		}
		spec.Children = append(spec.Children, res)
	}
	return spec, nil
}

// evalContext does an initial pass through the hcl.File f and returns a context with populated
// variables that can be used in the actual file evaluation
func evalContext(f *hcl.File) (*hcl.EvalContext, error) {
	var fi struct {
		Schemas []struct {
			Name string `hcl:",label"`
		} `hcl:"schema,block"`
		Tables []struct {
			Name    string `hcl:",label"`
			Columns []struct {
				Name   string   `hcl:",label"`
				Remain hcl.Body `hcl:",remain"`
			} `hcl:"column,block"`
			Remain hcl.Body `hcl:",remain"`
		} `hcl:"table,block"`
		Remain hcl.Body `hcl:",remain"`
	}
	if diag := gohcl.DecodeBody(f.Body, &hcl.EvalContext{}, &fi); diag.HasErrors() {
		return nil, diag
	}
	schemas := make(map[string]cty.Value)
	for _, sch := range fi.Schemas {
		ref, err := toSchemaRef(sch.Name)
		if err != nil {
			return nil, fmt.Errorf("schema: failed creating ref to schema %q", sch.Name)
		}
		schemas[sch.Name] = ref
	}
	tables := make(map[string]cty.Value)
	for _, tab := range fi.Tables {
		cols := make(map[string]cty.Value)
		for _, col := range tab.Columns {
			ref, err := toColumnRef(tab.Name, col.Name)
			if err != nil {
				return nil, fmt.Errorf("schema: failed ref for column %q in table %q", col.Name, tab.Name)
			}
			cols[col.Name] = ref
		}
		tables[tab.Name] = cty.ObjectVal(map[string]cty.Value{
			"column": cty.MapVal(cols),
		})
	}
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"reference_option": cty.MapVal(map[string]cty.Value{
				"no_action":   cty.StringVal(string(schema.NoAction)),
				"restrict":    cty.StringVal(string(schema.Restrict)),
				"cascade":     cty.StringVal(string(schema.Cascade)),
				"set_null":    cty.StringVal(string(schema.SetNull)),
				"set_default": cty.StringVal(string(schema.SetDefault)),
			}),
		},
	}
	if len(schemas) > 0 {
		ctx.Variables["schema"] = cty.MapVal(schemas)
	}
	if len(tables) > 0 {
		ctx.Variables["table"] = cty.MapVal(tables)
	}
	return ctx, nil
}

func toSchemaRef(name string) (cty.Value, error) {
	typ := cty.Object(map[string]cty.Type{
		"name": cty.String,
	})
	s := &schemaRef{Name: name}
	return gocty.ToCtyValue(s, typ)
}

func toColumnRef(table, column string) (cty.Value, error) {
	typ := cty.Object(map[string]cty.Type{
		"name":  cty.String,
		"table": cty.String,
	})
	c := columnRef{
		Name:  column,
		Table: table,
	}
	return gocty.ToCtyValue(c, typ)
}

type commonSpecParts struct {
	attrs    []*schemaspec.Attr
	children []*schemaspec.Resource
}

func extractCommon(ctx *hcl.EvalContext, remain hcl.Body, skip map[string]struct{}) (*commonSpecParts, error) {
	body, ok := remain.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("schemahcl: expected remainder to be of type *hclsyntax.Body")
	}
	attrs, err := toAttrs(ctx, body.Attributes, skip)
	if err != nil {
		return nil, err
	}
	common := &commonSpecParts{
		attrs: attrs,
	}
	for _, blk := range body.Blocks {
		if shouldSkip(skip, blk.Type) {
			continue
		}
		resource, err := toResource(ctx, blk)
		if err != nil {
			return nil, err
		}
		common.children = append(common.children, resource)
	}
	return common, nil
}