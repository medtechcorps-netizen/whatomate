package anonymous

type A struct{}

func (A) First() int { return 81 }

type B struct{}

func (B) Second() int { return 82 }

type C struct{}

func (C) Second() int { return 83 }
