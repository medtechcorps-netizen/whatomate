package entry

import (
	"closurefixture.test/callbacks"
	"closurefixture.test/dep"
	"closurefixture.test/runner"
)

func Root(value runner.Runner) int {
	var local dep.Number
	defer func() { local = dep.Number(dep.Deferred()) }()
	return int(local+dep.Worker{}.Value()) + value.Run() + dep.Box[int]{Value: 1}.Get() + (&dep.Pointer{}).Get() + callbacks.Both()
}
