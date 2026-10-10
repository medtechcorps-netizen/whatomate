package entry

import first "closurefixture.test/dep"

func ImportA() int { return first.Read() }
