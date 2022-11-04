package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
	"golang.org/x/tools/go/ast/astutil"
)

const (
	protoFilesPathSource = "api/proto/"
	generatedFiledPath   = "pkg/api/sdk/"
	protoExtension       = "proto"

	generatedFileTemplate     = "%s.pb.gw.go"
	rootFunctionTemplate      = "Register%sHandlerServer"
	methodFunctionTemplate    = "local_request_%s_%s_0"
	generatedFunctionTemplate = "%s_%s"

	errType             = "error"
	stringType          = "string"
	handlerResponseType = "handlerResponse"

	unaryServerInterceptorSelector  = "UnaryServerInterceptor"
	serverMetadataSelector          = "ServerMetadata"
	messageSelector                 = "Message"
	contextSelector                 = "Context"
	requestSelector                 = "Request"
	unaryServerInfoSelector         = "UnaryServerInfo"
	marshalerSelector               = "Marshaler"
	errorfSelector                  = "Errorf"
	annotateIncomingContextSelector = "AnnotateIncomingContext"

	handlerResponseItemVar = "handlerResponseItem"
	interceptorVar         = "interceptor"
	handlerVar             = "handler"
	mdVar                  = "md"
	respVar                = "resp"
	dataVar                = "data"
	okVar                  = "ok"
	reqVar                 = "req"
	errVar                 = "err"
	ctxVar                 = "ctx"
	annotatedContextVar    = "annotatedContext"
	inboundMarshalerVar    = "inboundMarshaler"
	nilVar                 = "nil"
	anyVar                 = "any"
	serverVar              = "server"
	pathParamsVar          = "pathParams"

	protoPackage   = "proto"
	runtimePackage = "runtime"
	grpcPackage    = "grpc"
	httpPackage    = "http"
	contextPackage = "context"
	fmtPackage     = "fmt"

	serverStructField     = "Server"
	fullMethodStructField = "FullMethod"
)

type assignmentWithRPCMethodName struct {
	rpcMethodName string
	assignStmt    *ast.AssignStmt
	funcName      string
}

type protoService struct {
	serviceName          string
	registerFunctionName string
	methods              []string
}

func (p *protoService) getMethodFunctionsMap() map[string]interface{} {
	resp := make(map[string]interface{})
	for i := range p.methods {
		resp[fmt.Sprintf(methodFunctionTemplate, p.serviceName, p.methods[i])] = nil
	}
	return resp
}

func getMethodsMap(in map[string]protoService) map[string]interface{} {
	resp := make(map[string]interface{})
	for i := range in {
		for j := range in[i].methods {
			resp[fmt.Sprintf(methodFunctionTemplate, in[i].serviceName, in[i].methods[j])] = nil
		}
	}
	return resp
}

func main() {
	logrus.Errorf("started")

	//input, _ := ioutil.ReadAll(os.Stdin)
	//var req pluginpb.CodeGeneratorRequest
	//proto.Unmarshal(file, &req)
	//
	return
	protoFiles, err := findProtoFilesInPath(protoFilesPathSource)
	if err != nil {
		logrus.Errorf("error getting proto files in path: %v", err)
		return
	}

	for i := range protoFiles {
		protoServices, err := getProtoServicesFromFile(protoFilesPathSource + protoFiles[i] + "." + protoExtension)
		if err != nil {
			logrus.Errorf("error getting proto services by filename: %v", err)
			return
		} else if len(protoServices) == 0 {
			continue
		}

		if protoFiles[i] == "chatops" {
			for j := range protoServices {
				fmt.Println(protoServices[j].methods)
			}
		}

		rootFunctions := getRootFunctionsNames(protoServices)
		currentFileMethods := getMethodsMap(rootFunctions)

		fSet := token.NewFileSet()

		file, err := parser.ParseFile(fSet, generatedFiledPath+fmt.Sprintf(generatedFileTemplate, protoFiles[i]), nil, parser.ParseComments)
		if err != nil {
			logrus.Errorf("error parsing go code from file: %v", err)
			return
		}

		var (
			previousRPCMethodName string
			serverType            string
			functions             = make(map[string]assignmentWithRPCMethodName)
		)

		astutil.Apply(
			file,
			nil,
			func(cursor *astutil.Cursor) bool {
				if funcDecl, ok := cursor.Node().(*ast.FuncDecl); ok {
					if funcDecl.Name != nil {
						if _, ok = rootFunctions[funcDecl.Name.Name]; ok {
							serverType = resolveServerType(funcDecl)
							if ok = checkIfFuncNeedField(funcDecl, interceptorVar); ok {
								funcDecl.Type.Params.List = append(funcDecl.Type.Params.List, interceptorField)
							}
						} else if _, ok = functions[funcDecl.Name.Name]; ok {
							cursor.Delete()
						}
					}
				}
				if assignStmt, ok := cursor.Node().(*ast.AssignStmt); ok {
					if len(assignStmt.Rhs) == 1 {

						if callExpr, ok := assignStmt.Rhs[0].(*ast.CallExpr); ok {

							if selectorExpr, ok := callExpr.Fun.(*ast.SelectorExpr); ok {
								if ident, ok := selectorExpr.X.(*ast.Ident); ok {
									if ident.Name == runtimePackage {
										if selectorExpr.Sel != nil && selectorExpr.Sel.Name == annotateIncomingContextSelector {
											for _, annotateArgs := range callExpr.Args {
												if basicLit, ok := annotateArgs.(*ast.BasicLit); ok {
													previousRPCMethodName = basicLit.Value
												}
											}
										}
									}
								}
							} else if funcIdent, ok := callExpr.Fun.(*ast.Ident); ok {

								newFunctionName := fmt.Sprintf(generatedFunctionTemplate, interceptorVar, funcIdent.Name)

								_, isCurrentFileMethod := currentFileMethods[funcIdent.Name]
								_, isNewGeneratedFunc := functions[newFunctionName]
								if isCurrentFileMethod || isNewGeneratedFunc {

									cursor.Replace(generateAssignmentStatement(newFunctionName))
									functions[newFunctionName] = assignmentWithRPCMethodName{
										rpcMethodName: previousRPCMethodName,
										assignStmt:    assignStmt,
										funcName:      newFunctionName,
									}
								}
							}
						}
					}
				}
				return true
			},
		)

		for _, val := range functions {
			file.Decls = append(file.Decls, generateFunctionDeclaration(val, serverType))
		}

		buf := bytes.NewBuffer(nil)

		astutil.AddImport(fSet, file, fmtPackage)

		if err = printer.Fprint(buf, fSet, file); err != nil {
			logrus.Errorf("error writing node to buffer: %v", err)
			return
		}

		if err = ioutil.WriteFile(generatedFiledPath+fmt.Sprintf(generatedFileTemplate, protoFiles[i]), buf.Bytes(), 0664); err != nil {
			logrus.Errorf("error writing file: %v", err)
			return
		}
	}
	return
}

func checkIfFuncNeedField(funcDecl *ast.FuncDecl, fieldName string) bool {
	if funcDecl == nil || funcDecl.Type == nil || funcDecl.Type.Params == nil {
		return false
	}
	for i := range funcDecl.Type.Params.List {
		fieldsMap := make(map[string]interface{})
		for _, val := range funcDecl.Type.Params.List[i].Names {
			fieldsMap[val.Name] = nil
		}
		if _, ok := fieldsMap[fieldName]; ok {
			return false
		}
	}
	return true
}

func resolveServerType(funcDecl *ast.FuncDecl) string {
	if funcDecl == nil || funcDecl.Type == nil || funcDecl.Type.Params == nil {
		return ""
	}
	for _, val := range funcDecl.Type.Params.List {
		for i := range val.Names {
			if val.Names[i].Name == serverVar {
				if ident, ok := val.Type.(*ast.Ident); ok {
					return ident.Name
				}
			}
		}
	}
	return ""
}

func getProtoServicesFromFile(filePath string) ([]protoService, error) {
	const (
		serviceKeyWord = "service"
		methodKeyWord  = "rpc"
	)
	var (
		resp              = make([]protoService, 0)
		nextAppendService = false
		nextAppendMethod  = false
	)

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error opening file: %w", err)
	}

	fields := strings.FieldsFunc(string(removeCTypeComments(data)), func(r rune) bool {
		if r == ' ' {

		}
		switch r {
		case ' ':
			return true
			//case '(':
			//	return false
			//case ')':
			//	return false
			//case ':':
			//	return false
		}
		return false
	})

	for _, value := range fields {
		if nextAppendService {
			nextAppendService = false
			resp = append(resp, protoService{serviceName: value})
		} else if nextAppendMethod {
			nextAppendMethod = false
			resp[len(resp)-1].methods = append(resp[len(resp)-1].methods, value)
		} else if value == serviceKeyWord {
			nextAppendService = true
		} else if value == methodKeyWord {
			nextAppendMethod = true
		}
	}
	return resp, nil
}

func removeCTypeComments(in []byte) []byte {
	re := regexp.MustCompile("(?s)//.*?\n|/\\*.*?\\*/")
	return re.ReplaceAll(in, nil)
}

func findProtoFilesInPath(path string) ([]string, error) {
	resp := make([]string, 0)

	dirInfo, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, err
	}

	for i := range dirInfo {
		if fileName, err := resolveProtoFileName(dirInfo[i].Name()); err != nil {
			continue
		} else {
			resp = append(resp, fileName)
		}
	}
	return resp, nil
}

func resolveProtoFileName(in string) (string, error) {
	items := strings.Split(in, ".")
	if len(items) < 2 {
		return "", fmt.Errorf("file have no extension")
	}
	if items[len(items)-1] == protoExtension {
		return strings.Join(items[:len(items)-1], "."), nil
	}
	return "", fmt.Errorf("file have no %s extension", protoExtension)
}

func getRootFunctionsNames(input []protoService) map[string]protoService {
	resp := make(map[string]protoService)

	for i := range input {
		service := protoService{
			serviceName:          input[i].serviceName,
			registerFunctionName: fmt.Sprintf(rootFunctionTemplate, input[i].serviceName),
			methods:              input[i].methods,
		}
		resp[fmt.Sprintf(rootFunctionTemplate, input[i].serviceName)] = service
	}
	return resp
}

func genIdent(in string) *ast.Ident {
	return &ast.Ident{
		Name: in,
	}
}

func genIdentWithObj(in string, kind ast.ObjKind) *ast.Ident {
	return &ast.Ident{
		Name: in,
		Obj: &ast.Object{
			Kind: kind,
			Name: in,
		},
	}
}

func generateAssignmentStatement(funcName string) *ast.AssignStmt {
	return &ast.AssignStmt{
		Tok: token.DEFINE,
		Lhs: []ast.Expr{
			genIdent(mdVar),
			genIdent(respVar),
			genIdent(errVar),
		},
		Rhs: []ast.Expr{
			&ast.CallExpr{
				Fun: genIdent(funcName),
				Args: []ast.Expr{
					genIdent(ctxVar),
					genIdent(annotatedContextVar),
					genIdent(inboundMarshalerVar),
					genIdent(serverVar),
					genIdent(interceptorVar),
					genIdent(reqVar),
					genIdent(pathParamsVar),
				},
			},
		},
	}
}

func generateField(pointer bool, packageName, selectorName string, names ...string) *ast.Field {
	var (
		fieldType ast.Expr
		name      []*ast.Ident
	)

	for i := range names {
		name = append(name, genIdent(names[i]))
	}
	if packageName == "" {
		fieldType = &ast.Ident{
			Name: selectorName,
		}
	} else {
		fieldType = &ast.SelectorExpr{
			X: &ast.Ident{
				Name: packageName,
			},
			Sel: &ast.Ident{
				Name: selectorName,
			},
		}
	}
	if pointer {
		fieldType = &ast.StarExpr{
			X: fieldType,
		}
	}

	return &ast.Field{
		Names: name,
		Type:  fieldType,
	}
}

func generateFunctionDeclaration(funcData assignmentWithRPCMethodName, serverType string) *ast.FuncDecl {
	return &ast.FuncDecl{
		Type: &ast.FuncType{
			Params: &ast.FieldList{
				List: []*ast.Field{
					generateField(false, contextPackage, contextSelector, ctxVar, annotatedContextVar),
					generateField(false, runtimePackage, marshalerSelector, inboundMarshalerVar),
					generateField(false, "", serverType, serverVar),
					generateField(true, grpcPackage, unaryServerInterceptorSelector, interceptorVar),
					generateField(true, httpPackage, requestSelector, reqVar),
					{
						Names: []*ast.Ident{
							genIdentWithObj(pathParamsVar, ast.Var),
						},
						Type: &ast.MapType{
							Key:   genIdent(stringType),
							Value: genIdent(stringType),
						},
					},
				},
			},
			Results: &ast.FieldList{
				List: []*ast.Field{
					generateField(false, runtimePackage, serverMetadataSelector, mdVar),
					generateField(false, protoPackage, messageSelector, respVar),
					generateField(false, "", errType, errVar),
				},
			},
		},
		Name: &ast.Ident{
			Name: funcData.funcName,
		},
		Body: &ast.BlockStmt{
			List: []ast.Stmt{
				&ast.DeclStmt{
					Decl: &ast.GenDecl{
						Tok: token.TYPE,
						Specs: []ast.Spec{
							&ast.TypeSpec{
								Name: genIdentWithObj(handlerResponseType, ast.Typ),
								Type: &ast.StructType{
									Fields: &ast.FieldList{
										List: []*ast.Field{
											generateField(false, runtimePackage, serverMetadataSelector, mdVar),
											generateField(false, protoPackage, messageSelector, respVar),
										},
									},
								},
							},
						},
					},
				},
				&ast.AssignStmt{
					Tok: token.DEFINE,
					Lhs: []ast.Expr{
						genIdentWithObj(handlerVar, ast.Var),
					},
					Rhs: []ast.Expr{
						&ast.FuncLit{
							Type: &ast.FuncType{
								Params: &ast.FieldList{
									List: []*ast.Field{
										generateField(false, contextPackage, contextSelector, ctxVar),
										generateField(false, "", anyVar, reqVar),
									},
								},
								Results: &ast.FieldList{
									List: []*ast.Field{
										generateField(false, "", anyVar),
										generateField(false, "", errType),
									},
								},
							},
							Body: &ast.BlockStmt{
								List: []ast.Stmt{
									&ast.IfStmt{
										Init: &ast.AssignStmt{
											Tok: token.DEFINE,
											Lhs: []ast.Expr{
												genIdentWithObj(reqVar, ast.Var),
												genIdentWithObj(okVar, ast.Var),
											},
											Rhs: []ast.Expr{
												&ast.TypeAssertExpr{
													X: genIdent(reqVar),
													Type: &ast.StarExpr{
														X: &ast.SelectorExpr{
															X:   genIdent(httpPackage),
															Sel: genIdent(requestSelector),
														},
													},
												},
											},
										},
										Cond: genIdent(okVar),
										Body: &ast.BlockStmt{
											List: []ast.Stmt{
												funcData.assignStmt,
												&ast.ReturnStmt{
													Results: []ast.Expr{
														&ast.CompositeLit{
															Type: genIdent(handlerResponseType),
															Elts: []ast.Expr{
																0: &ast.KeyValueExpr{
																	Key:   genIdent(respVar),
																	Value: genIdent(respVar),
																},
																&ast.KeyValueExpr{
																	Key:   genIdent(mdVar),
																	Value: genIdent(mdVar),
																},
															},
														},
														genIdent(errVar),
													},
												},
											},
										},
									},

									&ast.ReturnStmt{
										Results: []ast.Expr{
											genIdent(nilVar),
											&ast.CallExpr{
												Fun: &ast.SelectorExpr{
													X:   genIdent(fmtPackage),
													Sel: genIdent(errorfSelector),
												},
												Args: []ast.Expr{
													&ast.BasicLit{
														Kind:  token.STRING,
														Value: fmt.Sprintf("\"error converting req to *%s.Request\"", httpPackage),
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
				&ast.DeclStmt{
					Decl: &ast.GenDecl{
						Tok: token.VAR,
						Specs: []ast.Spec{
							&ast.ValueSpec{
								Names: []*ast.Ident{
									genIdentWithObj(handlerResponseItemVar, ast.Var),
								},
								Type: genIdent(anyVar),
							},
						},
					},
				},
				&ast.IfStmt{
					Cond: &ast.BinaryExpr{
						Op: token.EQL,
						X:  genIdent(interceptorVar),
						Y:  genIdent(nilVar),
					},
					Body: &ast.BlockStmt{
						List: []ast.Stmt{
							&ast.AssignStmt{
								Tok: token.ASSIGN,
								Lhs: []ast.Expr{
									genIdent(handlerResponseItemVar),
									genIdent(errVar),
								},
								Rhs: []ast.Expr{
									&ast.CallExpr{
										Fun: genIdent(handlerVar),
										Args: []ast.Expr{
											genIdent(ctxVar),
											genIdent(reqVar),
										},
									},
								},
							},
						},
					},
					Else: &ast.BlockStmt{
						List: []ast.Stmt{
							&ast.AssignStmt{
								Lhs: []ast.Expr{
									genIdent(handlerResponseItemVar),
									genIdent(errVar),
								},
								Tok: token.ASSIGN,
								Rhs: []ast.Expr{
									&ast.CallExpr{
										Fun: &ast.ParenExpr{
											X: &ast.StarExpr{
												X: genIdent(interceptorVar),
											},
										},
										Args: []ast.Expr{
											genIdent(ctxVar),
											genIdent(reqVar),
											&ast.UnaryExpr{
												Op: token.AND,
												X: &ast.CompositeLit{
													Type: &ast.SelectorExpr{
														X:   genIdent(grpcPackage),
														Sel: genIdent(unaryServerInfoSelector),
													},
													Elts: []ast.Expr{
														&ast.KeyValueExpr{
															Key:   genIdent(serverStructField),
															Value: genIdent(serverVar),
														},
														&ast.KeyValueExpr{
															Key: genIdent(fullMethodStructField),
															Value: &ast.BasicLit{
																Kind:  token.STRING,
																Value: funcData.rpcMethodName,
															},
														},
													},
												},
											},
											genIdent(handlerVar),
										},
									},
								},
							},
						},
					},
				},
				&ast.IfStmt{
					Cond: &ast.BinaryExpr{
						Op: token.NEQ,
						X:  genIdent(errVar),
						Y:  genIdent(nilVar),
					},
					Body: &ast.BlockStmt{
						List: []ast.Stmt{
							&ast.ReturnStmt{
								Results: []ast.Expr{},
							},
						},
					},
				},
				&ast.AssignStmt{
					Lhs: []ast.Expr{
						genIdentWithObj(dataVar, ast.Var),
						genIdentWithObj(okVar, ast.Var),
					},
					Tok: token.DEFINE,
					Rhs: []ast.Expr{
						&ast.TypeAssertExpr{
							X:    genIdent(handlerResponseItemVar),
							Type: genIdent(handlerResponseType),
						},
					},
				},
				&ast.IfStmt{
					Cond: &ast.UnaryExpr{
						Op: token.NOT,
						X:  genIdent(okVar),
					},
					Body: &ast.BlockStmt{
						List: []ast.Stmt{
							&ast.ReturnStmt{},
						},
					},
				},
				&ast.ReturnStmt{
					Results: []ast.Expr{
						&ast.SelectorExpr{
							X:   genIdent(dataVar),
							Sel: genIdent(mdVar),
						},
						&ast.SelectorExpr{
							X:   genIdent(dataVar),
							Sel: genIdent(respVar),
						},
						genIdent(nilVar),
					},
				},
			},
		},
	}
}

var (
	interceptorField = &ast.Field{
		Names: []*ast.Ident{
			genIdentWithObj(interceptorVar, ast.Var),
		},
		Type: &ast.StarExpr{
			X: &ast.SelectorExpr{
				X:   genIdent(grpcPackage),
				Sel: genIdent(unaryServerInterceptorSelector),
			},
		},
	}
)
