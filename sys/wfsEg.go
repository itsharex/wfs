// Copyright (c) 2023, donnie <donnie4w@gmail.com>
// All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// github.com/donnie4w/wfs
//

package sys

var (
	KeyStoreInit func(string)
	Count        func() int64
	Seq          func() int64
	AppendData   func(string, []byte, int32) ERROR
	GetData      func(string) []byte
	DelData      func(string) ERROR
	Add          func([]byte, []byte) error
	Del          func([]byte) error
	SearchLike   func(string) []*PathBean
	SearchLimit  func(int64, int64) []*PathBean
	Defrag       func(string) ERROR
	FragAnalysis func(string) (*FragBean, ERROR)
)
