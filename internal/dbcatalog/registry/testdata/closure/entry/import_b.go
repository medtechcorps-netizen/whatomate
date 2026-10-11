package entry

import second "closurefixture.test/dep"

func ImportB() int { return second.Read() }
