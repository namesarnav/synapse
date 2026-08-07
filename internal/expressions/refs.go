package expressions

import "sort"

// Refs is the static dependency footprint of an expression.
type Refs struct {
	Nodes   []string // node ids read via nodes.<id> or nodes["id"]
	Secrets []string // secret names read via secrets.NAME
	Roots   []string // root variables used (trigger, nodes, item...)
	Dynamic bool     // a node/secret was looked up with a non-literal key
}

// References statically analyses the expression without evaluating it.
func (e *Expr) References() Refs {
	c := &refCollector{nodes: map[string]bool{}, secrets: map[string]bool{}, roots: map[string]bool{}}
	c.walk(e.root)
	return c.result()
}

// TemplateReferences analyses every expression inside a template.
func (t *Template) References() Refs {
	c := &refCollector{nodes: map[string]bool{}, secrets: map[string]bool{}, roots: map[string]bool{}}
	for _, p := range t.parts {
		if p.expr != nil {
			c.walk(p.expr.root)
		}
	}
	return c.result()
}

type refCollector struct {
	nodes, secrets, roots map[string]bool
	dynamic               bool
}

func (c *refCollector) result() Refs {
	r := Refs{Dynamic: c.dynamic}
	for k := range c.nodes {
		r.Nodes = append(r.Nodes, k)
	}
	for k := range c.secrets {
		r.Secrets = append(r.Secrets, k)
	}
	for k := range c.roots {
		r.Roots = append(r.Roots, k)
	}
	sort.Strings(r.Nodes)
	sort.Strings(r.Secrets)
	sort.Strings(r.Roots)
	return r
}

func (c *refCollector) walk(n node) {
	switch t := n.(type) {
	case identNode:
		c.roots[t.name] = true
	case memberNode:
		if id, ok := t.obj.(identNode); ok {
			switch id.name {
			case "nodes":
				c.roots["nodes"] = true
				c.nodes[t.name] = true
				return
			case "secrets":
				c.roots["secrets"] = true
				c.secrets[t.name] = true
				return
			}
		}
		c.walk(t.obj)
	case indexNode:
		if id, ok := t.obj.(identNode); ok && (id.name == "nodes" || id.name == "secrets") {
			c.roots[id.name] = true
			set := c.nodes
			if id.name == "secrets" {
				set = c.secrets
			}
			if lit, ok := t.idx.(litNode); ok {
				if s, ok := lit.val.(string); ok {
					set[s] = true
					return
				}
			}
			c.dynamic = true
			c.walk(t.idx)
			return
		}
		c.walk(t.obj)
		c.walk(t.idx)
	case unaryNode:
		c.walk(t.x)
	case binaryNode:
		c.walk(t.l)
		c.walk(t.r)
	case ternaryNode:
		c.walk(t.cond)
		c.walk(t.a)
		c.walk(t.b)
	case callNode:
		for _, a := range t.args {
			c.walk(a)
		}
	case arrayNode:
		for _, e := range t.elems {
			c.walk(e)
		}
	case objectNode:
		for _, v := range t.vals {
			c.walk(v)
		}
	}
}

// Check performs static validation beyond syntax: known functions and arity.
func (e *Expr) Check() error { return checkNode(e.root) }

func checkNode(n node) error {
	switch t := n.(type) {
	case callNode:
		f, ok := funcs[t.name]
		if !ok {
			return &Error{"syntax", t.pos, "unknown function " + t.name + "()"}
		}
		if len(t.args) < f.min || (f.max >= 0 && len(t.args) > f.max) {
			return &Error{"syntax", t.pos, t.name + "() takes " + arity(f) + " arguments"}
		}
		for _, a := range t.args {
			if err := checkNode(a); err != nil {
				return err
			}
		}
	case memberNode:
		return checkNode(t.obj)
	case indexNode:
		if err := checkNode(t.obj); err != nil {
			return err
		}
		return checkNode(t.idx)
	case unaryNode:
		return checkNode(t.x)
	case binaryNode:
		if err := checkNode(t.l); err != nil {
			return err
		}
		return checkNode(t.r)
	case ternaryNode:
		for _, c := range []node{t.cond, t.a, t.b} {
			if err := checkNode(c); err != nil {
				return err
			}
		}
	case arrayNode:
		for _, e := range t.elems {
			if err := checkNode(e); err != nil {
				return err
			}
		}
	case objectNode:
		for _, v := range t.vals {
			if err := checkNode(v); err != nil {
				return err
			}
		}
	}
	return nil
}
