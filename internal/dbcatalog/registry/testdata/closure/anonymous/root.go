package anonymous

type Pair interface {
	First() int
	Second() int
}

func Root(value Pair) int { return value.First() + value.Second() }

func Factory() any {
	return struct {
		A
		B
	}{}
}
func LocalFactory() any {
	type Local struct {
		A
		C
	}
	return Local{}
}
