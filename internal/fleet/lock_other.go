//go:build !linux

package fleet

import "errors"

func Lock(string) (func(), error) {
	return nil, errors.New("fleet enrollment and updates require Linux amd64")
}
