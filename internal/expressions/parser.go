package expressions

const maxDepth = 48

// Expr is a parsed expression, safe for concurrent evaluation.
type Expr struct {
	src  string
	root node
}

func (e *Expr) Source() string { return e.src }

// Parse parses src into an Expr.
func Parse(src string) (*Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	root, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, synErr(t.pos, "unexpected %s", describe(t))
	}
	return &Expr{src: src, root: root}, nil
}

type parser struct {
	toks  []token
	i     int
	depth int
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func describe(t token) string {
	switch t.kind {
	case tEOF:
		return "end of expression"
	case tStr:
		return "string"
	case tNum:
		return "number " + t.text
	}
	return "'" + t.text + "'"
}

func (p *parser) isPunct(s string) bool { t := p.peek(); return t.kind == tPunct && t.text == s }

func (p *parser) expectPunct(s string) (token, error) {
	t := p.next()
	if t.kind != tPunct || t.text != s {
		return t, synErr(t.pos, "expected '%s' but found %s", s, describe(t))
	}
	return t, nil
}

// binary operator table: token -> precedence (higher binds tighter).
func binPrec(t token) (string, int) {
	if t.kind == tPunct {
		switch t.text {
		case "??":
			return "??", 2
		case "||":
			return "||", 3
		case "&&":
			return "&&", 4
		case "==", "!=":
			return t.text, 5
		case "<", "<=", ">", ">=":
			return t.text, 6
		case "+", "-":
			return t.text, 7
		case "*", "/", "%":
			return t.text, 8
		}
	}
	if t.kind == tIdent {
		switch t.text {
		case "or":
			return "||", 3
		case "and":
			return "&&", 4
		case "in":
			return "in", 5
		}
	}
	return "", 0
}

func (p *parser) enter(pos int) error {
	p.depth++
	if p.depth > maxDepth {
		return &Error{"limit", pos, "expression nested too deeply"}
	}
	return nil
}

func (p *parser) expr(minPrec int) (node, error) {
	if err := p.enter(p.peek().pos); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if minPrec <= 1 && t.kind == tPunct && t.text == "?" {
			p.next()
			a, err := p.expr(1)
			if err != nil {
				return nil, err
			}
			if _, err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			b, err := p.expr(1)
			if err != nil {
				return nil, err
			}
			left = ternaryNode{pos: t.pos, cond: left, a: a, b: b}
			continue
		}
		op, prec := binPrec(t)
		if prec == 0 || prec < minPrec || prec < 2 {
			return left, nil
		}
		p.next()
		next := prec + 1
		if op == "??" { // right associative
			next = prec
		}
		right, err := p.expr(next)
		if err != nil {
			return nil, err
		}
		left = binaryNode{pos: t.pos, op: op, l: left, r: right}
	}
}

func (p *parser) unary() (node, error) {
	t := p.peek()
	if t.kind == tPunct && (t.text == "!" || t.text == "-") || t.kind == tIdent && t.text == "not" {
		p.next()
		if err := p.enter(t.pos); err != nil {
			return nil, err
		}
		defer func() { p.depth-- }()
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		op := t.text
		if op == "not" {
			op = "!"
		}
		return unaryNode{pos: t.pos, op: op, x: x}, nil
	}
	return p.postfix()
}

func (p *parser) postfix() (node, error) {
	n, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.kind == tPunct && (t.text == "." || t.text == "?."):
			p.next()
			name := p.next()
			if name.kind != tIdent {
				return nil, synErr(name.pos, "expected property name after '%s'", t.text)
			}
			n = memberNode{pos: t.pos, obj: n, name: name.text, optional: t.text == "?."}
		case t.kind == tPunct && t.text == "[":
			p.next()
			idx, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			if _, err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			n = indexNode{pos: t.pos, obj: n, idx: idx}
		default:
			return n, nil
		}
	}
}

func (p *parser) primary() (node, error) {
	t := p.next()
	switch t.kind {
	case tNum:
		return litNode{t.pos, t.num}, nil
	case tStr:
		return litNode{t.pos, t.text}, nil
	case tIdent:
		switch t.text {
		case "true":
			return litNode{t.pos, true}, nil
		case "false":
			return litNode{t.pos, false}, nil
		case "null":
			return litNode{t.pos, nil}, nil
		case "and", "or", "not", "in":
			return nil, synErr(t.pos, "unexpected keyword '%s'", t.text)
		}
		if p.isPunct("(") {
			p.next()
			var args []node
			if !p.isPunct(")") {
				for {
					a, err := p.expr(0)
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.isPunct(",") {
						p.next()
						continue
					}
					break
				}
			}
			if _, err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return callNode{pos: t.pos, name: t.text, args: args}, nil
		}
		return identNode{t.pos, t.text}, nil
	case tPunct:
		switch t.text {
		case "(":
			n, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			if _, err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return n, nil
		case "[":
			var elems []node
			for !p.isPunct("]") {
				e, err := p.expr(0)
				if err != nil {
					return nil, err
				}
				elems = append(elems, e)
				if p.isPunct(",") {
					p.next()
					continue
				}
				break
			}
			if _, err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			return arrayNode{pos: t.pos, elems: elems}, nil
		case "{":
			obj := objectNode{pos: t.pos}
			for !p.isPunct("}") {
				k := p.next()
				if k.kind != tIdent && k.kind != tStr {
					return nil, synErr(k.pos, "expected object key but found %s", describe(k))
				}
				if _, err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.expr(0)
				if err != nil {
					return nil, err
				}
				obj.keys = append(obj.keys, k.text)
				obj.vals = append(obj.vals, v)
				if p.isPunct(",") {
					p.next()
					continue
				}
				break
			}
			if _, err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return obj, nil
		}
	}
	return nil, synErr(t.pos, "unexpected %s", describe(t))
}
