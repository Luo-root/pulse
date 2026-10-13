package pulse

import "reflect"

// 类型检查管声明，实际值检查穿透 interface。沿引用记身份，避免自指 map/slice/指针无限递归。
// 不调用 Interface、用户方法或 Codec，也不复制业务对象；与 Set 的值一样，遍历期间不能并发改值。
type checkpointValueVisit struct {
	typ    reflect.Type
	ptr    uintptr
	length int
}

func checkpointChannelValue(v reflect.Value, seen map[checkpointValueVisit]bool) bool {
	if !v.IsValid() {
		return false
	}
	if v.Kind() == reflect.Chan {
		return true
	}
	if v.Kind() == reflect.Interface {
		return !v.IsNil() && checkpointChannelValue(v.Elem(), seen)
	}
	var visit checkpointValueVisit
	switch v.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		if v.IsNil() {
			return false
		}
		visit.typ = v.Type()
		if v.Kind() == reflect.Map {
			visit.ptr = uintptr(v.UnsafePointer())
		} else {
			visit.ptr = v.Pointer()
		}
		if v.Kind() == reflect.Slice {
			visit.length = v.Len() // 同一底层数组的不同长度视图不能互相遮住后半段。
		}
		if seen[visit] {
			return false
		}
		if seen == nil {
			seen = make(map[checkpointValueVisit]bool)
		}
		seen[visit] = true
	}
	switch v.Kind() {
	case reflect.Pointer:
		return checkpointChannelValue(v.Elem(), seen)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if checkpointChannelValue(v.Index(i), seen) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if checkpointChannelValue(iter.Key(), seen) || checkpointChannelValue(iter.Value(), seen) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if checkpointChannelValue(v.Field(i), seen) {
				return true
			}
		}
	}
	return false
}
