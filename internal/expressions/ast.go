package expressions

// node is an AST node. Every node records its source offset for errors.
type node interface{ position() int }

type (
	litNode struct {
		pos int
		val any
	}
	identNode struct {
		pos  int
		name string
	}
	memberNode struct { // obj.name and obj?.name
		pos      int
		obj      node
		name     string
		optional bool
	}
	indexNode struct {
		pos int
		obj node
		idx node
	}
	callNode struct {
		pos  int
		name string
		args []node
	}
	unaryNode struct {
		pos int
		op  string
		x   node
	}
	binaryNode struct {
		pos  int
		op   string
		l, r node
	}
	ternaryNode struct {
		pos        int
		cond, a, b node
	}
	arrayNode struct {
		pos   int
		elems []node
	}
	objectNode struct {
		pos  int
		keys []string
		vals []node
	}
)

func (n litNode) position() int     { return n.pos }
func (n identNode) position() int   { return n.pos }
func (n memberNode) position() int  { return n.pos }
func (n indexNode) position() int   { return n.pos }
func (n callNode) position() int    { return n.pos }
func (n unaryNode) position() int   { return n.pos }
func (n binaryNode) position() int  { return n.pos }
func (n ternaryNode) position() int { return n.pos }
func (n arrayNode) position() int   { return n.pos }
func (n objectNode) position() int  { return n.pos }
