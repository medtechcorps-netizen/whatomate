package dynamic

import "reflect"

func Parameter(fn func() int) int { return fn() }

type Callback struct{ Call func() int }

func Field(v Callback) int { return v.Call() }

var callback = func() int { return 1 }

func Variable() int { return callback() }

func Reflection(v reflect.Value) { v.Call(nil) }
