package dep

type Worker struct{}

func (Worker) Value() Number { return Number(Read()) }
