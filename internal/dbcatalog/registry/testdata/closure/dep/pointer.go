package dep

type Pointer struct{}

func (*Pointer) Get() int { return 31 }
