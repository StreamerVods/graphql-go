package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/graph-gophers/graphql-go/errors"
	"github.com/graph-gophers/graphql-go/internal/common"
	"github.com/graph-gophers/graphql-go/internal/exec/resolvable"
	"github.com/graph-gophers/graphql-go/internal/exec/selected"
	"github.com/graph-gophers/graphql-go/internal/query"
)

type Response struct {
	Data   json.RawMessage
	Errors []*errors.QueryError
}

func (r *Request) Subscribe(ctx context.Context, s *resolvable.Schema, op *query.Operation) <-chan *Response {
	var result reflect.Value
	var f *selected.SchemaField
	var err *errors.QueryError
	rawErr := func() error {
		defer r.handlePanic(ctx)
		sels := selected.ApplyOperation(&r.Request, s, op)
		var resolver reflect.Value
		if s.ResolverInfo.IsFunc {
			args := []reflect.Value{}
			if s.ResolverInfo.HasContext {
				ctx = contextWithSelectedFields(ctx, sels)
				args = append(args, reflect.ValueOf(ctx))
			}
			if s.ResolverInfo.HasVariables {
				args = append(args, reflect.ValueOf(r.Vars))
			}
			vals := s.Resolver.Call(args)
			if len(vals) > 1 {
				err := vals[1].Interface()
				if err != nil {
					switch t := err.(type) {
					case error:
						return t
					default:
						return fmt.Errorf("%v", err)
					}
				}
			}
			resolver = vals[0]
			if resolver.IsNil() {
				return fmt.Errorf("resolver is nil")
			}
		} else {
			resolver = s.Resolver
		}

		var fields []*fieldToExec
		collectFieldsToResolve(sels, s, resolver, &fields, make(map[string]*fieldToExec))

		// TODO: move this check into validation.Validate
		if len(fields) != 1 {
			err = errors.Errorf("%s", "can subscribe to at most one subscription at a time")
			return nil
		}
		f = fields[0].field

		switch f.Type.(type) {
		case *common.RootResolver:
			if len(f.Sels) != 1 {
				err = errors.Errorf("%s", "can subscribe to at most one subscription at a time")
				return nil
			}
			if f.MethodIndex != -1 {
				m := resolver.Method(f.MethodIndex)
				mtype := m.Type()
				count := mtype.NumIn()
				vals := []reflect.Value{}
				if count != 0 {
					vals = append(vals, reflect.ValueOf(ctx))
					count--
				}
				if count != 0 {
					return fmt.Errorf("too many parameters")
				}
				outs := m.Call(vals)
				count = len(outs)
				if count == 0 {
					return fmt.Errorf("resolver is nil")
				}
				resolver = outs[0]
				if count == 2 {
					err := vals[1].Interface()
					if err != nil {
						switch t := err.(type) {
						case error:
							return t
						default:
							return fmt.Errorf("%v", err)
						}
					}
				}
			} else {
				resolver = resolver.Elem().FieldByIndex(f.FieldIndex)
			}
			if resolver.IsNil() {
				return fmt.Errorf("resolver is nil")
			}
			f = f.Sels[0].(*selected.SchemaField)
		}

		var in []reflect.Value
		if f.HasContext {
			ctx = contextWithSelectedFields(ctx, f.Sels)
			in = append(in, reflect.ValueOf(ctx))
		}
		if f.ArgsPacker != nil {
			in = append(in, f.PackedArgs)
		}

		callOut := resolver.Method(f.MethodIndex).Call(in)
		result = callOut[0]

		if f.HasError && !callOut[1].IsNil() {
			switch resolverErr := callOut[1].Interface().(type) {
			case *errors.QueryError:
				err = resolverErr
			case error:
				err = errors.Errorf("%s", resolverErr)
				err.ResolverError = resolverErr
			default:
				panic(fmt.Errorf("can only deal with *QueryError and error types, got %T", resolverErr))
			}
		}
		return nil
	}()

	if rawErr != nil {
		return sendAndReturnClosed(&Response{Errors: []*errors.QueryError{errors.Errorf("%s", rawErr)}})
	}

	// Handles the case where the locally executed func above panicked
	if len(r.Request.Errs) > 0 {
		return sendAndReturnClosed(&Response{Errors: r.Request.Errs})
	}

	if f == nil {
		return sendAndReturnClosed(&Response{Errors: []*errors.QueryError{err}})
	}

	if err != nil {
		if _, nonNullChild := f.Type.(*common.NonNull); nonNullChild {
			return sendAndReturnClosed(&Response{Errors: []*errors.QueryError{err}})
		}
		return sendAndReturnClosed(&Response{Data: []byte(fmt.Sprintf(`{"%s":null}`, f.Alias)), Errors: []*errors.QueryError{err}})
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return sendAndReturnClosed(&Response{Errors: []*errors.QueryError{errors.Errorf("%s", ctxErr)}})
	}

	c := make(chan *Response)
	// TODO: handle resolver nil channel better?
	if result == reflect.Zero(result.Type()) {
		close(c)
		return c
	}

	go func() {
		for {
			// Check subscription context
			chosen, resp, ok := reflect.Select([]reflect.SelectCase{
				{
					Dir:  reflect.SelectRecv,
					Chan: reflect.ValueOf(ctx.Done()),
				},
				{
					Dir:  reflect.SelectRecv,
					Chan: result,
				},
			})
			switch chosen {
			// subscription context done
			case 0:
				close(c)
				return
			// upstream received
			case 1:
				// upstream closed
				if !ok {
					close(c)
					return
				}

				subR := &Request{
					Request: selected.Request{
						Doc:    r.Request.Doc,
						Vars:   r.Request.Vars,
						Schema: r.Request.Schema,
					},
					Limiter: r.Limiter,
					Tracer:  r.Tracer,
					Logger:  r.Logger,
				}
				var out bytes.Buffer
				func() {
					timeout := r.SubscribeResolverTimeout
					if timeout == 0 {
						timeout = time.Second
					}

					subCtx, cancel := context.WithTimeout(ctx, timeout)
					defer cancel()

					// resolve response
					func() {
						defer subR.handlePanic(subCtx)

						var buf bytes.Buffer
						subR.execSelectionSet(subCtx, f.Sels, f.Type, &pathSegment{nil, f.Alias}, s, resp, &buf)

						propagateChildError := false
						if _, nonNullChild := f.Type.(*common.NonNull); nonNullChild && resolvedToNull(&buf) {
							propagateChildError = true
						}

						if !propagateChildError {
							out.WriteString(fmt.Sprintf(`{"%s":`, f.Alias))
							out.Write(buf.Bytes())
							out.WriteString(`}`)
						}
					}()

					if err := subCtx.Err(); err != nil {
						c <- &Response{Errors: []*errors.QueryError{errors.Errorf("%s", err)}}
						return
					}

					// Send response within timeout
					// TODO: maybe block until sent?
					select {
					case <-subCtx.Done():
					case c <- &Response{Data: out.Bytes(), Errors: subR.Errs}:
					}
				}()
			}
		}
	}()

	return c
}

func sendAndReturnClosed(resp *Response) chan *Response {
	c := make(chan *Response, 1)
	c <- resp
	close(c)
	return c
}
