package dep

type Box[T ~int] struct{ Value T }

func (v Box[T]) Get() T { return v.Value }
