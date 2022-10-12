// SPDX-License-Identifier: BSD-3-Clause
//
// Authors: Alexander Jung <alex@unikraft.io>
//
// Copyright (c) 2022, Unikraft GmbH.  All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions
// are met:
//
// 1. Redistributions of source code must retain the above copyright
//    notice, this list of conditions and the following disclaimer.
// 2. Redistributions in binary form must reproduce the above copyright
//    notice, this list of conditions and the following disclaimer in the
//    documentation and/or other materials provided with the distribution.
// 3. Neither the name of the copyright holder nor the names of its
//    contributors may be used to endorse or promote products derived from
//    this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
// AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
// IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
// ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
// LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
// CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
// SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
// CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
// ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
// POSSIBILITY OF SUCH DAMAGE.

package main

import (
	"io"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/golang/glog"
	"github.com/iancoleman/strcase"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Options are the options to set for rendering the template.
type Options struct {
	EmitEmpty            bool
	EmitMessageOptions   bool
	EmitAnyAsGeneric     bool
	EmitEnumPrefix       bool
	RemapEnumViaJsonName bool
	MapEnumToMessage     bool
}

type header struct {
	*protogen.File
	Options
	HasService bool
	HasEnumMap bool
}

type service struct {
	*protogen.Service
	Options
}

type enum struct {
	*protogen.Enum
	JSONNames map[string]string
	Options
	MapValues map[string]string
}

func (e enum) ToCamel(name protoreflect.Name) string {
	if strings.ToUpper(string(name)) == string(name) {
		return "_" + string(name)
	}

	return strcase.ToCamel(string(name))
}

type messageExtraField struct {
	GoName   string
	JSONName string
	Value    string
	Kind     protoreflect.Kind
}

type message struct {
	Message     *protogen.Message
	Options     Options
	ExtraFields []messageExtraField
}

func (e message) ToCamel(name string) string {
	return strcase.ToCamel(name)
}

func (m message) KindToGoType(kind protoreflect.Kind) (typ string) {
	switch kind {
	case protoreflect.BoolKind:
		typ = "bool"
	case protoreflect.Int32Kind:
		typ = "int32"
	case protoreflect.Sint32Kind:
		typ = "int32"
	case protoreflect.Uint32Kind:
		typ = "uint32"
	case protoreflect.Int64Kind:
		typ = "int64"
	case protoreflect.Sint64Kind:
		typ = "int64"
	case protoreflect.Uint64Kind:
		typ = "uint64"
	case protoreflect.Sfixed32Kind:
		typ = "int64"
	case protoreflect.Fixed32Kind:
		typ = "uint32"
	case protoreflect.FloatKind:
		typ = "float32"
	case protoreflect.Sfixed64Kind:
		typ = "int64"
	case protoreflect.Fixed64Kind:
		typ = "uint64"
	case protoreflect.DoubleKind:
		typ = "float64"
	case protoreflect.StringKind:
		typ = "string"
	case protoreflect.BytesKind:
		typ = "[]byte"
	default:
		typ = "any"
	}

	return
}

func (m message) FieldToGoType(field protogen.Field) (typ string) {
	kind := field.Desc.Kind()
	switch kind {
	case protoreflect.EnumKind:
		typ = strcase.ToCamel(field.Enum.GoIdent.GoName)
	case protoreflect.MessageKind:
		typ = strcase.ToCamel(field.Message.GoIdent.GoName)
	default:
		typ = m.KindToGoType(kind)
	}

	if typ == "" {
		glog.V(2).Infof("skipping %s, unsupported field type %s", field.Desc.FullName(), field.Desc.Kind().String())
		return
	}

	if field.Desc.IsList() {
		typ = "[]" + typ
	}

	return
}

type method struct {
	*protogen.Method
	Options
	ServiceGoName string
}

var (
	headerTemplate = template.Must(template.New("header").Parse(HeaderTemplate))
	HeaderTemplate = `
// Code generated by kraftkit.sh/tools/protoc-gen-go-netconn. DO NOT EDIT.
// source: {{ .Proto.Name }}

package {{.GoPackageName}}
{{ if (or .HasService .HasEnumMap) }}
import (
{{ if .HasService }}
	"bufio"
	"encoding/json"
	"fmt"
	"io"
{{- end }}
	"reflect"
{{ if .HasService -}}
	"sync"
{{ end }}
)
{{ end }}
`

	serviceTemplate = template.Must(template.New("service").Parse(ServiceTemplate))
	ServiceTemplate = `
type {{ .GoName }}Client struct {
	conn io.ReadWriteCloser
	lock sync.RWMutex
	recv *bufio.Reader
	send *bufio.Writer
}

func New{{ .GoName }}Client(conn io.ReadWriteCloser) *{{ .GoName }}Client {
	return &{{ .GoName }}Client{
		conn: conn,
		recv: bufio.NewReader(conn),
		send: bufio.NewWriter(conn),
	}
}

func (c *{{ .GoName }}Client) Close() error {
	return c.conn.Close()
}

func (c *{{ .GoName }}Client) setRpcRequestSetDefaults(face any) error {
	v := reflect.ValueOf(face)

	// If it's an interface or a pointer, unwrap it.
	if v.Kind() == reflect.Ptr && v.Elem().Kind() == reflect.Struct {
		v = v.Elem()
	} else {
		return nil
	}

	t := reflect.TypeOf(v.Interface())

	for i := 0; i < v.NumField(); i++ {
		def := t.Field(i).Tag.Get("default")
		if def == "" {
			continue
		}

		f := v.FieldByName(t.Field(i).Name)
		if !f.IsValid() || !f.CanSet() {
			continue
		}

		switch f.Kind() {
		case reflect.String:
			f.SetString(def)
		default:
			return fmt.Errorf("unsupported default kind: %s", f.Kind().String())
		}
	}

	return nil
}
`

	messageTemplate = template.Must(template.New("message").Parse(MessageTemplate))
	MessageTemplate = `
{{ $this := . }}
{{ $tick := "` + "`" + `" }}
{{ if .Message.Comments.Leading -}}
{{ .Message.Comments.Leading -}}
{{ end -}}
type {{ .ToCamel .Message.GoIdent.GoName }} struct {
{{ range $field := $this.ExtraFields }}
{{ $type := $this.KindToGoType $field.Kind -}}
{{ if ne $type "" -}}
{{ $field.GoName }} {{ $type }} {{ $tick }}json:"{{ $field.JSONName }}"
	{{- if ne $field.Value "" }} default:"{{ $field.Value }}"{{ end }}{{ $tick }}
{{ end }}
{{ end -}}
{{ range $field := .Message.Fields -}}
{{ if $field.Comments.Leading -}}
	{{ $field.Comments.Leading -}}
{{ end -}}
{{ $type := "any" -}}
{{ if not (and ($field.Message) (eq $field.Message.Desc.FullName "google.protobuf.Any")) -}}
{{ $type = $this.FieldToGoType $field -}}
{{ end -}}
{{ if ne $type "" -}}
{{ $this.ToCamel $field.GoName }} {{ $type }} {{ $tick }}json:"{{ $field.Desc.JSONName }}"{{ $tick }}
{{ end -}}
{{ end -}}
}
`

	enumTemplate = template.Must(template.New("enum").Funcs(sprig.TxtFuncMap()).Parse(EnumTemplate))
	EnumTemplate = `
{{ $this := . }}
{{ if .Enum.Comments.Leading -}}
{{ .Enum.Comments.Leading -}}
{{ end -}}
type {{ .Enum.GoIdent.GoName }} string
const (
	{{ range $field := .Enum.Values -}}
		{{ if $field.Comments.Leading -}}
			{{ $field.Comments.Leading -}}
		{{ end -}}
		{{ $field.GoIdent.GoName }} = {{ $this.Enum.GoIdent.GoName }}("{{ with (index $this.JSONNames ( toString $field.Desc.Name )) }}{{ . }}{{ else }}{{ $field.Desc.Name }}{{ end }}")
	{{ end -}}
)

func (e {{ .Enum.GoIdent.GoName }}) String() string {
	return string(e)
}

func {{ .Enum.GoIdent.GoName }}s() []{{ .Enum.GoIdent.GoName }} {
	return []{{ .Enum.GoIdent.GoName }}{
		{{ range $field := .Enum.Values -}}
		{{ $field.GoIdent.GoName }},
		{{ end }}
	}
}

{{ if (and .Options.MapEnumToMessage (ne (len .MapValues) 0)) }}
func {{ .Enum.GoIdent.GoName }}TypeMap() map[{{ .Enum.GoIdent.GoName }}]reflect.Type {
	return map[{{ .Enum.GoIdent.GoName }}]reflect.Type{
		{{ range $key, $val := .MapValues -}}
		{{ $key }}: reflect.TypeOf({{ $val }}{}),
		{{ end }}
	}
}
{{ end }}
`

	methodTemplate = template.Must(template.New("method").Parse(MethodTemplate))
	MethodTemplate = `
{{ $hasReq := or (ne .Input.Desc.FullName "google.protobuf.Empty") (and .EmitEmpty (eq .Input.Desc.FullName "google.protobuf.Empty")) }}
{{ $hasRes := or (ne .Output.Desc.FullName "google.protobuf.Empty") (and .EmitEmpty (eq .Output.Desc.FullName "google.protobuf.Empty")) }}
{{ $resAsAny := and (eq .Output.Desc.FullName "google.protobuf.Any") .EmitAnyAsGeneric }}
func (c *{{ .ServiceGoName }}Client) {{ .GoName }}(
	{{- if $hasReq -}}
	req {{ .Input.GoIdent.GoName -}}
	{{ end -}}
) ({{ if and $hasRes $resAsAny }}*any, {{ else if $hasRes }}*{{ .Output.GoIdent.GoName }}, {{ end }}error) {
	var b []byte
	var err error

	c.lock.Lock()
	defer c.lock.Unlock()

	{{ if $hasReq }}
	if err := c.setRpcRequestSetDefaults(&req); err != nil {
		return {{ if $hasRes }}nil, {{ end }}err
	}
	{{ end }}

	{{ if $hasReq }}
	b, err = json.Marshal(req)
	if err != nil {
		return {{ if $hasRes }}nil, {{ end }}err
	}
	if _, err := c.send.Write(append(b, '\x0a')); err != nil {
		return {{ if $hasRes }}nil, {{ end }}err
	}
	if err := c.send.Flush(); err != nil {
		return {{ if $hasRes }}nil, {{ end }}err
	}
	{{ end }}

	{{ if $hasRes }}
	var res {{ if $resAsAny }}any{{ else }}{{ .Output.GoIdent.GoName }}{{ end }}
	b, err = c.recv.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}

	return &res, nil
	{{ else }}
	return nil
	{{ end -}}
}
`
)

func applyEnums(w io.Writer, enums []*protogen.Enum, opts Options) error {
	for _, e := range enums {
		if e.Desc.IsPlaceholder() {
			glog.V(2).Infof("Skipping placeholder enum %s", e.GoIdent.GoName)
		}

		jsonNames := make(map[string]string)
		mapVals := make(map[string]string)

		glog.V(2).Infof("Processing enum %s", e.GoIdent.GoName)

		for i, ev := range e.Values {
			// Temporarily remove the parent prefix name from the GoIdent so we can
			// later re-apply it in case its value has changed
			e.Values[i].GoIdent.GoName = strings.TrimPrefix(e.Values[i].GoIdent.GoName, e.Values[i].Parent.GoIdent.GoName+"_")

			desc := protodesc.ToEnumValueDescriptorProto(ev.Desc)
			if desc.Options != nil && (opts.RemapEnumViaJsonName || opts.MapEnumToMessage) {
				options := ev.Desc.Options().(*descriptorpb.EnumValueOptions)
				b, err := proto.Marshal(options)
				if err != nil {
					return err
				}

				options.Reset()
				err = proto.UnmarshalOptions{Resolver: extTypes}.Unmarshal(b, options)
				if err != nil {
					return err
				}

				options.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
					if !fd.IsExtension() {
						return true
					}

					if fd.Name() == "json_name" && opts.RemapEnumViaJsonName {
						jsonNames[string(ev.Desc.Name())] = v.String()
					}
					if fd.Name() == "map_message" && opts.MapEnumToMessage {
						mapVals[string(ev.Desc.Name())] = v.String()
					}

					return true
				})
			}

			//
			if opts.EmitEnumPrefix {
				e.Values[i].GoIdent.GoName = e.Values[i].Parent.GoIdent.GoName + "_" + e.Values[i].GoIdent.GoName
			}
		}

		if err := enumTemplate.Execute(w, enum{
			e, jsonNames, opts, mapVals,
		}); err != nil {
			return err
		}
	}

	return nil
}

func applyMessages(w io.Writer, messages []*protogen.Message, opts Options) error {
	for _, m := range messages {
		if m.Desc.IsMapEntry() {
			glog.V(2).Infof("Skipping mapentry message %s", m.GoIdent.GoName)
			continue
		}

		var extraFields []messageExtraField

		desc := protodesc.ToDescriptorProto(m.Desc)
		if desc.Options != nil && opts.EmitMessageOptions {
			options := m.Desc.Options().(*descriptorpb.MessageOptions)
			b, err := proto.Marshal(options)
			if err != nil {
				return err
			}

			options.Reset()
			err = proto.UnmarshalOptions{Resolver: extTypes}.Unmarshal(b, options)
			if err != nil {
				return err
			}

			// Use protobuf reflection to iterate over all the extension fields,
			// looking for the ones that we are interested in.
			options.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
				if !fd.IsExtension() {
					return true
				}

				if fd.Kind() == protoreflect.MessageKind {
					glog.V(2).Infof("Skipping option %s as protoc-gen-go-netconn does not support options with custom message types", fd.Name())
					return true
				}

				extraFields = append(extraFields, messageExtraField{
					GoName:   strcase.ToCamel(string(fd.Name())),
					JSONName: string(fd.Name()),
					Value:    v.String(),
					Kind:     fd.Kind(),
				})

				return true
			})
		}

		glog.V(2).Infof("Processing message %s", m.GoIdent.GoName)

		if err := messageTemplate.Execute(w, message{
			Message:     m,
			Options:     opts,
			ExtraFields: extraFields,
		}); err != nil {
			return err
		}

		if err := applyMessages(w, m.Messages, opts); err != nil {
			return err
		}
	}

	return nil
}

// ApplyTemplate accepts an input proto file and emits each service method,
// enums and messages.
func ApplyTemplate(w io.Writer, f *protogen.File, opts Options) error {
	hasService := false
	hasEnumMap := false

	for _, s := range f.Services {
		if s.Desc.IsPlaceholder() {
			continue
		}

		hasService = true
		break
	}

	if opts.MapEnumToMessage {
	loop:
		for _, e := range f.Enums {
			for _, ev := range e.Values {
				desc := protodesc.ToEnumValueDescriptorProto(ev.Desc)
				if desc.Options != nil {
					options := ev.Desc.Options().(*descriptorpb.EnumValueOptions)
					b, err := proto.Marshal(options)
					if err != nil {
						return err
					}

					options.Reset()
					err = proto.UnmarshalOptions{Resolver: extTypes}.Unmarshal(b, options)
					if err != nil {
						return err
					}

					options.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
						if !fd.IsExtension() {
							return true
						}

						if fd.Name() == "map_message" {
							hasEnumMap = true
						}

						return true
					})

					if hasEnumMap {
						break loop
					}
				}
			}
		}
	}

	if err := headerTemplate.Execute(w, header{
		f, opts, hasService, hasEnumMap,
	}); err != nil {
		return err
	}

	for _, s := range f.Services {
		if s.Desc.IsPlaceholder() {
			glog.V(2).Infof("Skipping placeholder service %s", s.GoName)
			continue
		}

		glog.V(2).Infof("Processing service %s", s.GoName)

		if err := serviceTemplate.Execute(w, service{
			s, opts,
		}); err != nil {
			return err
		}

		for _, m := range s.Methods {
			if err := methodTemplate.Execute(w, method{
				Method:        m,
				Options:       opts,
				ServiceGoName: s.GoName,
			}); err != nil {
				return err
			}
		}
	}

	if err := applyEnums(w, f.Enums, opts); err != nil {
		return err
	}

	if err := applyMessages(w, f.Messages, opts); err != nil {
		return err
	}

	return nil
}