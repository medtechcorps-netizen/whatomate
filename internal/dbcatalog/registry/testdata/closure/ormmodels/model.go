package ormmodels

import "gorm.io/gorm"
import "gorm.io/gorm/schema"

type Model struct{ Base }

func (Model) TableName() string            { return "fixture_rows" }
func (*Model) BeforeCreate(*gorm.DB) error { return nil }
func (Model) String() string               { return "excluded string" }

type Base struct{}

func (*Base) AfterFind(*gorm.DB) error { return nil }

type Named struct{}

func (Named) TableName(schema.Namer) string { return "named_rows" }

type Unreached struct{}

func (Unreached) TableName() string { return "unreached_rows" }

type Wrong struct{}

func (Wrong) BeforeCreate(string) error { return nil }
