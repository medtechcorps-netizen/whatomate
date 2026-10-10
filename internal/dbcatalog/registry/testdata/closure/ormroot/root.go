package ormroot

import "closurefixture.test/ormmodels"
import _ "gorm.io/gorm/callbacks"

func Root() any { return []any{ormmodels.Model{}, ormmodels.Named{}, ormmodels.Wrong{}} }
