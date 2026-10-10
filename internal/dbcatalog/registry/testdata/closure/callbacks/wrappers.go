package callbacks

func Apply(fn func() int) int { return fn() }

func Forward(fn func() int) int { return Apply(fn) }

func Recursive(fn func() int, depth int) int {
	if depth == 0 {
		return fn()
	}
	return Recursive(fn, depth-1)
}

func Both() int {
	return Forward(First) + Forward(Second) + Recursive(First, 1) + Apply(func() int { return Third() }) + Local()
}

func Unknown(fn func() int) int { return Forward(First) + Forward(fn) }

func Local() int {
	rows := func() int { return First() }
	load := func() int { return rows() + Second() }
	return Apply(func() int { return load() }) + rows()
}

func Reassigned(fn func() int) int       { fn = Second; return fn() }
func Escaped(fn func() int) int          { pointer := &fn; *pointer = Second; return fn() }
func NestedAssignment(fn func() int) int { func() { fn = Second }(); return fn() }
func RangeAssignment(fn func() int) int {
	for _, fn = range []func() int{Second} {
	}
	return fn()
}
func BadReassigned() int { return Reassigned(First) }
func BadEscaped() int    { return Escaped(First) }
func BadNested() int     { return NestedAssignment(First) }
func BadRange() int      { return RangeAssignment(First) }
func BadLocal() int      { fn := func() int { return First() }; fn = Second; return fn() }
func BadLocalEscape() int {
	fn := func() int { return First() }
	pointer := &fn
	*pointer = Second
	return fn()
}
