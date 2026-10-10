package unrelated

type Other struct{}

func (Other) String() string { return "unrelated" }

func (Other) Run() string { return "wrong signature" }
