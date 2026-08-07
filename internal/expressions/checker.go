package expressions

import "fmt"

// Checker adapts the expression package to workflow.ExprChecker.
type Checker struct {
	// Cron validates a cron spec; nil accepts any non-empty spec.
	Cron func(spec, tz string) error
}

func (c Checker) CheckExpression(src string) ([]string, error) {
	e, err := Parse(src)
	if err != nil {
		return nil, err
	}
	if err := e.Check(); err != nil {
		return nil, err
	}
	return refsOrErr(e.References())
}

func (c Checker) CheckTemplate(src string) ([]string, error) {
	t, err := ParseTemplate(src)
	if err != nil {
		return nil, err
	}
	for _, p := range t.parts {
		if p.expr != nil {
			if err := p.expr.Check(); err != nil {
				return nil, err
			}
		}
	}
	return refsOrErr(t.References())
}

func (c Checker) CheckCron(spec, tz string) error {
	if c.Cron != nil {
		return c.Cron(spec, tz)
	}
	return nil
}

func refsOrErr(r Refs) ([]string, error) {
	if r.Dynamic {
		return r.Nodes, fmt.Errorf("nodes and secrets must be referenced by a literal name")
	}
	return r.Nodes, nil
}
