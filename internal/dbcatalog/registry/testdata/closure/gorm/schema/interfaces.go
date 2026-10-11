package schema

type Namer interface{ TableName(string) string }
type Tabler interface{ TableName() string }
type TablerWithNamer interface{ TableName(Namer) string }
