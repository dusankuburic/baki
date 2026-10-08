package database

import "strconv"

// sqlArgs is a statement's bind arguments in placeholder order. add appends one and returns its placeholder,
// so a statement assembled from optional parts never numbers its parameters by hand. It spreads into
// ExecContext/QueryContext as it is (args...).
type sqlArgs []any

// add appends v and returns its placeholder ("$1" for the first argument).
func (a *sqlArgs) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}
