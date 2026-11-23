package calc

import "fmt"

type node interface{ position() int }

type (
	numLit struct {
		p int
		v num
	}
	strLit struct {
		p int
		s string
	}
	boolLit struct {
		p int
		v bool
	}
	identN struct {
		p    int
		name string
	}
	unaryN struct {
		p  int
		op string
		x  node
	}
	binaryN struct {
		p    int
		op   string
		l, r node
	}
	factN struct {
		p int
		x node
	}
	callN struct {
		p    int
		fn   node
		args []node
	}
	// indexN is x[idx] or x[idx, col]; col is nil for one index.
	indexN struct {
		p           int
		x, idx, col node
	}
	// allN is a lone `:` inside brackets: every row or every column.
	allN struct {
		p int
	}
	// transN is the postfix transpose x'.
	transN struct {
		p int
		x node
	}
	listN struct {
		p     int
		items []node
	}
	rangeN struct {
		p                int
		start, step, end node
	} // step may be nil
	lambdaN struct {
		p      int
		params []string
		body   node
	}
	assignN struct {
		p    int
		name string
		x    node
	}
	defineN struct {
		p      int
		name   string
		params []string
		body   node
	}
)

func (n *numLit) position() int  { return n.p }
func (n *strLit) position() int  { return n.p }
func (n *boolLit) position() int { return n.p }
func (n *identN) position() int  { return n.p }
func (n *unaryN) position() int  { return n.p }
func (n *binaryN) position() int { return n.p }
func (n *factN) position() int   { return n.p }
func (n *callN) position() int   { return n.p }
func (n *indexN) position() int  { return n.p }
func (n *allN) position() int    { return n.p }
func (n *transN) position() int  { return n.p }
func (n *listN) position() int   { return n.p }
func (n *rangeN) position() int  { return n.p }
func (n *lambdaN) position() int { return n.p }
func (n *assignN) position() int { return n.p }
func (n *defineN) position() int { return n.p }

// keywords may not be assigned and are not looked up as variables.
var keywords = map[string]bool{"and": true, "or": true, "not": true, "true": true, "false": true, "if": true}

type parser struct {
	toks  []token
	i     int
	depth int
}

// parse turns src into a list of statements.
func parse(src string) ([]node, *Error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var stmts []node
	for {
		for p.peek().kind == tSep {
			p.i++
		}
		if p.peek().kind == tEOF {
			break
		}
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, s)
		switch t := p.peek(); {
		case t.kind == tSep || t.kind == tEOF:
		default:
			return nil, p.unexpected(t, "the end of the statement")
		}
	}
	if len(stmts) == 0 {
		return nil, errf(codeEmptyStatement, 0, "the expression is empty").withHint("write an expression such as `2 + 2`")
	}
	return stmts, nil
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) peekAt(k int) token {
	if p.i+k < len(p.toks) {
		return p.toks[p.i+k]
	}
	return p.toks[len(p.toks)-1]
}

func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) isOp(s string) bool {
	t := p.peek()
	return t.kind == tOp && t.s == s
}

func (p *parser) isIdent(s string) bool {
	t := p.peek()
	return t.kind == tIdent && t.s == s
}

func (p *parser) expect(s string, what string) (token, *Error) {
	if !p.isOp(s) {
		return token{}, p.unexpected(p.peek(), what)
	}
	return p.next(), nil
}

// unexpected builds the syntax error for token t where want was expected,
// with a hint for the common slips a model makes.
func (p *parser) unexpected(t token, want string) *Error {
	got := describeTok(t)
	e := errf(codeSyntax, t.pos, "expected %s, found %s", want, got)
	switch {
	case t.kind == tOp && t.s == "=":
		e.Hint = "use `==` to compare; `=` assigns and only at the start of a statement"
	case t.kind == tNum || t.kind == tIdent || t.kind == tStr || t.kind == tOp && t.s == "(":
		e.Hint = "no implicit multiplication: write `*` between factors"
	case t.kind == tOp && t.s == "'":
		e.Hint = "strings use double quotes, \"text\"; `'` after a matrix is its transpose, A'"
	}
	return e
}

func describeTok(t token) string {
	switch t.kind {
	case tEOF:
		return "the end of the expression"
	case tSep:
		return "the end of the statement"
	case tStr:
		return "a string"
	}
	return "`" + t.s + "`"
}

func (p *parser) statement() (node, *Error) {
	start := p.peek()
	x, err := p.expr()
	if err != nil {
		return nil, err
	}
	if !p.isOp("=") {
		return x, nil
	}
	eq := p.next()
	switch lhs := x.(type) {
	case *identN:
		if err := checkName(lhs.name, lhs.p); err != nil {
			return nil, err
		}
		rhs, err := p.expr()
		if err != nil {
			return nil, err
		}
		if lam, ok := rhs.(*lambdaN); ok && callsName(lam.body, lhs.name) {
			return nil, selfCallErr(lhs.name, lhs.p)
		}
		return &assignN{p: start.pos, name: lhs.name, x: rhs}, nil
	case *callN:
		id, ok := lhs.fn.(*identN)
		if !ok {
			break
		}
		params, perr := paramNames(lhs.args)
		if perr != nil {
			return nil, perr
		}
		if err := checkName(id.name, id.p); err != nil {
			return nil, err
		}
		body, err := p.expr()
		if err != nil {
			return nil, err
		}
		if callsName(body, id.name) {
			return nil, selfCallErr(id.name, id.p)
		}
		return &defineN{p: start.pos, name: id.name, params: params, body: body}, nil
	}
	return nil, errf(codeSyntax, eq.pos, "cannot assign to this expression").
		withHint("use `==` to compare; assign with `name = ...` or define with `f(x) = ...`")
}

func recursionErr(name string, pos int) *Error {
	return errf(codeRecursion, pos, "%s calls itself; recursion is not supported", name).
		withHint("use a closed form, or map/reduce over a range, e.g. reduce(1:n, (a, b) -> a*b)")
}

// selfCallErr is recursionErr for a definition whose body calls its own
// name. Inside the user's own sum, sum(...) is the user's sum (a call
// takes the user's function first), so a model that meant the built-in
// is told to rename. Kept apart from recursionErr, which the builtin
// table reaches through call, so builtinIndex is not an init cycle.
func selfCallErr(name string, pos int) *Error {
	if _, ok := builtinIndex[name]; ok {
		return errf(codeRecursion, pos, "%s calls itself; recursion is not supported", name).
			withHint("your " + name + " shadows the built-in " + name +
				" for calls, also inside its own body; give yours another name, e.g. my_" + name)
	}
	return recursionErr(name, pos)
}

func paramNames(args []node) ([]string, *Error) {
	names := make([]string, 0, len(args))
	seen := map[string]bool{}
	for _, a := range args {
		id, ok := a.(*identN)
		if !ok {
			return nil, errf(codeSyntax, a.position(), "a function parameter must be a name").
				withHint("define as `f(x, y) = x + y`; use `==` to compare")
		}
		if err := checkName(id.name, id.p); err != nil {
			return nil, err
		}
		if seen[id.name] {
			return nil, errf(codeSyntax, id.p, "parameter %s appears twice", id.name)
		}
		seen[id.name] = true
		names = append(names, id.name)
	}
	return names, nil
}

// checkName refuses keywords and constants as the name of a variable, a
// function, a parameter or a var. A built-in function name is allowed:
// variables and functions are separate namespaces (evalCall), and the
// constants stay reserved because a silently rebound `e` or `pi` gives
// wrong math with no error.
func checkName(name string, pos int) *Error {
	if keywords[name] || isConstant(name) {
		return errf(codeReservedName, pos, "%s is reserved and cannot be a name", name)
	}
	return nil
}

// callsName reports whether n contains a call to the plain name (outside a
// lambda that rebinds it as a parameter).
func callsName(n node, name string) bool {
	found := false
	walk(n, func(m node) bool {
		switch m := m.(type) {
		case *lambdaN:
			for _, p := range m.params {
				if p == name {
					return false
				}
			}
		case *callN:
			if id, ok := m.fn.(*identN); ok && id.name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// walk visits n and its children depth-first while visit returns true.
func walk(n node, visit func(node) bool) {
	if n == nil || !visit(n) {
		return
	}
	switch n := n.(type) {
	case *unaryN:
		walk(n.x, visit)
	case *binaryN:
		walk(n.l, visit)
		walk(n.r, visit)
	case *factN:
		walk(n.x, visit)
	case *callN:
		walk(n.fn, visit)
		for _, a := range n.args {
			walk(a, visit)
		}
	case *indexN:
		walk(n.x, visit)
		walk(n.idx, visit)
		walk(n.col, visit)
	case *transN:
		walk(n.x, visit)
	case *listN:
		for _, a := range n.items {
			walk(a, visit)
		}
	case *rangeN:
		walk(n.start, visit)
		walk(n.step, visit)
		walk(n.end, visit)
	case *lambdaN:
		walk(n.body, visit)
	}
}

func (p *parser) enter() *Error {
	p.depth++
	if p.depth > MaxDepth {
		return errf(codeTooDeep, p.peek().pos, "the expression nests deeper than %d levels", MaxDepth)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

// expr is the lowest precedence level: or.
func (p *parser) expr() (node, *Error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.isIdent("or") {
		t := p.next()
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = &binaryN{p: t.pos, op: "or", l: l, r: r}
	}
	return l, nil
}

func (p *parser) and() (node, *Error) {
	l, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.isIdent("and") {
		t := p.next()
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		l = &binaryN{p: t.pos, op: "and", l: l, r: r}
	}
	return l, nil
}

func (p *parser) not() (node, *Error) {
	if p.isIdent("not") {
		t := p.next()
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		return &unaryN{p: t.pos, op: "not", x: x}, nil
	}
	return p.comparison()
}

var cmpOps = map[string]bool{"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true}

func (p *parser) comparison() (node, *Error) {
	l, err := p.rangeExpr()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind != tOp || !cmpOps[t.s] {
		return l, nil
	}
	p.next()
	r, err := p.rangeExpr()
	if err != nil {
		return nil, err
	}
	if t2 := p.peek(); t2.kind == tOp && cmpOps[t2.s] {
		return nil, errf(codeSyntax, t2.pos, "comparisons do not chain").withHint("write `a < b and b < c`")
	}
	return &binaryN{p: t.pos, op: t.s, l: l, r: r}, nil
}

// rangeExpr binds looser than arithmetic so `1:n+1` is 1 to n+1.
func (p *parser) rangeExpr() (node, *Error) {
	a, err := p.additive()
	if err != nil {
		return nil, err
	}
	if !p.isOp(":") {
		return a, nil
	}
	t := p.next()
	b, err := p.additive()
	if err != nil {
		return nil, err
	}
	if !p.isOp(":") {
		return &rangeN{p: t.pos, start: a, end: b}, nil
	}
	p.next()
	c, err := p.additive()
	if err != nil {
		return nil, err
	}
	return &rangeN{p: t.pos, start: a, step: b, end: c}, nil
}

func (p *parser) additive() (node, *Error) {
	l, err := p.multiplicative()
	if err != nil {
		return nil, err
	}
	for p.isOp("+") || p.isOp("-") {
		t := p.next()
		r, err := p.multiplicative()
		if err != nil {
			return nil, err
		}
		l = &binaryN{p: t.pos, op: t.s, l: l, r: r}
	}
	return l, nil
}

func (p *parser) multiplicative() (node, *Error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.isOp("*") || p.isOp("/") || p.isOp("%") || p.isOp(".*") || p.isOp("./") {
		t := p.next()
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		l = &binaryN{p: t.pos, op: t.s, l: l, r: r}
	}
	return l, nil
}

// unary binds looser than ^, so -2^2 is -(2^2) = -4, as on paper.
func (p *parser) unary() (node, *Error) {
	if p.isOp("-") || p.isOp("+") {
		t := p.next()
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &unaryN{p: t.pos, op: t.s, x: x}, nil
	}
	return p.power()
}

// power is right-associative: its exponent is parsed by unary, which comes
// back here, so 2^3^2 is 2^(3^2) and 2^-1 is allowed.
func (p *parser) power() (node, *Error) {
	base, err := p.postfix()
	if err != nil {
		return nil, err
	}
	if !p.isOp("^") && !p.isOp(".^") {
		return base, nil
	}
	t := p.next()
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	exp, err := p.unary()
	if err != nil {
		return nil, err
	}
	return &binaryN{p: t.pos, op: t.s, l: base, r: exp}, nil
}

func (p *parser) postfix() (node, *Error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.kind == tOp && t.s == "(":
			if _, isNum := x.(*numLit); isNum {
				return nil, errf(codeSyntax, t.pos, "a number cannot be called").withHint("no implicit multiplication: write `*` before `(`")
			}
			p.next()
			args, err := p.list(")")
			if err != nil {
				return nil, err
			}
			x = &callN{p: t.pos, fn: x, args: args}
		case t.kind == tOp && t.s == "[":
			p.next()
			ix, err := p.index(t.pos, x)
			if err != nil {
				return nil, err
			}
			x = ix
		case t.kind == tOp && t.s == "!":
			p.next()
			x = &factN{p: t.pos, x: x}
		case t.kind == tOp && t.s == "'":
			p.next()
			x = &transN{p: t.pos, x: x}
		default:
			return x, nil
		}
	}
}

// index parses the rest of x[i] or x[i, j] after the `[`.
func (p *parser) index(pos int, x node) (node, *Error) {
	idx, err := p.indexPart()
	if err != nil {
		return nil, err
	}
	var col node
	if p.isOp(",") {
		p.next()
		if col, err = p.indexPart(); err != nil {
			return nil, err
		}
	}
	if _, err := p.expect("]", "`]`"); err != nil {
		return nil, err
	}
	return &indexN{p: pos, x: x, idx: idx, col: col}, nil
}

// indexPart is one index: an expression, or a lone `:` for all of them.
func (p *parser) indexPart() (node, *Error) {
	if t := p.peek(); t.kind == tOp && t.s == ":" {
		if n := p.peekAt(1); n.kind == tOp && (n.s == "," || n.s == "]") {
			p.next()
			return &allN{p: t.pos}, nil
		}
	}
	return p.expr()
}

// list parses comma-separated expressions up to close; a trailing comma is
// allowed.
func (p *parser) list(close string) ([]node, *Error) {
	var items []node
	for !p.isOp(close) {
		it, err := p.expr()
		if err != nil {
			return nil, err
		}
		items = append(items, it)
		if p.isOp(",") {
			p.next()
			continue
		}
		if !p.isOp(close) {
			return nil, p.unexpected(p.peek(), fmt.Sprintf("`,` or `%s`", close))
		}
	}
	p.next()
	return items, nil
}

func (p *parser) primary() (node, *Error) {
	t := p.peek()
	switch t.kind {
	case tNum:
		p.next()
		return &numLit{p: t.pos, v: t.num}, nil
	case tStr:
		p.next()
		return &strLit{p: t.pos, s: t.s}, nil
	case tIdent:
		switch t.s {
		case "true", "false":
			p.next()
			return &boolLit{p: t.pos, v: t.s == "true"}, nil
		case "and", "or", "not":
			return nil, errf(codeSyntax, t.pos, "`%s` needs an operand before it", t.s)
		}
		if n := p.peekAt(1); n.kind == tOp && n.s == "->" {
			if err := checkName(t.s, t.pos); err != nil {
				return nil, err
			}
			p.i += 2
			return p.lambdaBody(t.pos, []string{t.s})
		}
		p.next()
		return &identN{p: t.pos, name: t.s}, nil
	case tOp:
		switch t.s {
		case "(":
			if p.parenIsLambda() {
				p.next()
				args, err := p.list(")")
				if err != nil {
					return nil, err
				}
				params, err := paramNames(args)
				if err != nil {
					return nil, err
				}
				p.next() // ->
				return p.lambdaBody(t.pos, params)
			}
			p.next()
			x, err := p.expr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(")", "`)`"); err != nil {
				return nil, err
			}
			return x, nil
		case "[":
			p.next()
			items, err := p.list("]")
			if err != nil {
				return nil, err
			}
			return &listN{p: t.pos, items: items}, nil
		}
	}
	return nil, p.unexpected(t, "a number, a name, `(` or `[`")
}

// parenIsLambda looks past the matching ')' for '->'.
func (p *parser) parenIsLambda() bool {
	depth := 0
	for k := p.i; k < len(p.toks); k++ {
		t := p.toks[k]
		if t.kind != tOp {
			if t.kind == tEOF {
				return false
			}
			continue
		}
		switch t.s {
		case "(", "[":
			depth++
		case ")", "]":
			depth--
			if depth == 0 {
				n := p.toks[k+1]
				return n.kind == tOp && n.s == "->"
			}
		}
	}
	return false
}

func (p *parser) lambdaBody(pos int, params []string) (node, *Error) {
	body, err := p.expr()
	if err != nil {
		return nil, err
	}
	return &lambdaN{p: pos, params: params, body: body}, nil
}
