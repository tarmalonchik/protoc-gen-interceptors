package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"golang.org/x/tools/go/ast/astutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

const (
	protoExtension = ".proto"

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
	methods              []*descriptorpb.MethodDescriptorProto
}

type protoFile struct {
	filename string
	services []*descriptorpb.ServiceDescriptorProto
}

func getMethodsMap(in map[string]protoService) map[string]interface{} {
	resp := make(map[string]interface{})
	for i := range in {
		for j := range in[i].methods {
			resp[fmt.Sprintf(methodFunctionTemplate, in[i].serviceName, in[i].methods[j].GetName())] = nil
		}
	}
	return resp
}

func stringToMap(in []string) map[string]interface{} {
	resp := make(map[string]interface{})
	for i := range in {
		resp[in[i]] = nil
	}
	return resp
}

func resolveProtoFilesFromCodeGeneratorRequest(req *pluginpb.CodeGeneratorRequest) (resp []protoFile) {
	protoFilesMap := stringToMap(req.FileToGenerate)
	protoFilesParsed := req.GetProtoFile()
	for _, file := range protoFilesParsed {
		if len(file.GetService()) == 0 {
			continue
		}
		if _, ok := protoFilesMap[file.GetName()]; ok {
			resp = append(resp, protoFile{
				filename: file.GetName(),
				services: file.GetService(),
			})
		}
	}
	return resp
}

func resolveOutDir(in string) string {
	items := strings.Split(in, "=")
	if len(items) == 2 {
		return items[1]
	}
	return ""
}

func main() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		logrus.Errorf("reading stdin error: %v", err)
		return
	}

	req := &pluginpb.CodeGeneratorRequest{}
	if err = proto.Unmarshal(data, req); err != nil {
		logrus.Errorf("unmarshal error %v", err)
		return
	}
	outDir := resolveOutDir(req.GetParameter())

	protoFileList := resolveProtoFilesFromCodeGeneratorRequest(req)

	for i := range protoFileList {
		processSingleProto(&protoFileList[i], outDir)
	}
	return
}

func processSingleProto(singleFile *protoFile, outDir string) {
	var (
		lastRPCMethodName string
		serverType        string
		functions         = make(map[string]assignmentWithRPCMethodName)
	)

	if singleFile == nil {
		return
	}

	rootFunctions := getRootFunctionsNames(*singleFile)

	currentFileMethods := getMethodsMap(rootFunctions)

	fSet := token.NewFileSet()
	generatedFileName := fmt.Sprintf("%s/%s", outDir, fmt.Sprintf(generatedFileTemplate, resolveProtoFileName(singleFile.filename)))

	fileAst, err := parser.ParseFile(
		fSet,
		generatedFileName,
		nil,
		parser.ParseComments,
	)
	if err != nil {
		logrus.Errorf("error parsing go code from file: %v", err)
		return
	}

	astutil.Apply(
		fileAst,
		nil,
		func(cursor *astutil.Cursor) bool {
			if funcDecl, ok := cursor.Node().(*ast.FuncDecl); ok {
				if funcDecl.Name != nil {
					// checking if the function is root
					if _, ok = rootFunctions[funcDecl.Name.Name]; ok {
						serverType = resolveServerType(funcDecl)
						if ok = checkIfFuncNeedField(funcDecl, interceptorVar); ok {
							// adding new field to root function
							funcDecl.Type.Params.List = append(funcDecl.Type.Params.List, getInterceptorField())
						}
					} else if _, ok = functions[funcDecl.Name.Name]; ok {
						// we need to delete old functions generated by this package to add them again later
						cursor.Delete()
					}
				}
			}
			if assignStmt, ok := cursor.Node().(*ast.AssignStmt); ok {
				if len(assignStmt.Rhs) == 1 {
					if callExpr, ok := assignStmt.Rhs[0].(*ast.CallExpr); ok {
						if selectorExpr, ok := callExpr.Fun.(*ast.SelectorExpr); ok {
							// we need if when generating functions added to the end of file
							tryToExtractRPCMethodName(&lastRPCMethodName, selectorExpr, callExpr)
						} else if funcIdent, ok := callExpr.Fun.(*ast.Ident); ok {
							newFunctionName := fmt.Sprintf(generatedFunctionTemplate, interceptorVar, funcIdent.Name)
							// should replace old function call with new one which will be generated at the end of file
							_, isCurrentFileMethod := currentFileMethods[funcIdent.Name]
							_, isNewGeneratedFunc := functions[newFunctionName]
							if isCurrentFileMethod || isNewGeneratedFunc {
								cursor.Replace(generateAssignmentStatement(newFunctionName))
								functions[newFunctionName] = assignmentWithRPCMethodName{
									rpcMethodName: lastRPCMethodName,
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

	// adding functions to the end of the generated files
	for _, val := range functions {
		fileAst.Decls = append(fileAst.Decls, generateFunctionDeclaration(val, serverType))
	}

	buf := bytes.NewBuffer(nil)

	astutil.AddImport(fSet, fileAst, fmtPackage)

	if err = printer.Fprint(buf, fSet, fileAst); err != nil {
		logrus.Errorf("error writing node to buffer: %v", err)
		return
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		logrus.Errorf("error formatting generated code: %v", err)
		return
	}

	if err = printer.Fprint(buf, fSet, fileAst); err != nil {
		logrus.Errorf("error formatting: %v", err)
		return
	}

	if err = os.WriteFile(generatedFileName, formatted, 0664); err != nil { // nolint
		logrus.Errorf("error writing file: %v", err)
		return
	}
}

func tryToExtractRPCMethodName(rpcMethodName *string, selectorExpr *ast.SelectorExpr, callExpr *ast.CallExpr) {
	if rpcMethodName == nil || selectorExpr == nil || callExpr == nil {
		return
	}
	if ident, ok := selectorExpr.X.(*ast.Ident); ok {
		if ident.Name == runtimePackage {
			if selectorExpr.Sel != nil && selectorExpr.Sel.Name == annotateIncomingContextSelector {
				for _, annotateArgs := range callExpr.Args {
					if basicLit, ok := annotateArgs.(*ast.BasicLit); ok {
						*rpcMethodName = basicLit.Value
					}
				}
			}
		}
	}
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

func resolveProtoFileName(in string) string {
	return strings.ReplaceAll(in, protoExtension, "")
}

func getRootFunctionsNames(input protoFile) map[string]protoService {
	resp := make(map[string]protoService)

	for i := range input.services {
		service := protoService{
			serviceName:          input.services[i].GetName(),
			registerFunctionName: fmt.Sprintf(rootFunctionTemplate, input.services[i].GetName()),
			methods:              input.services[i].GetMethod(),
		}
		resp[fmt.Sprintf(rootFunctionTemplate, input.services[i].GetName())] = service
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
		Lhs: exprToList(genIdent(mdVar), genIdent(respVar), genIdent(errVar)),
		Rhs: exprToList(
			getCallExpr(
				genIdent(funcName),
				genIdent(ctxVar),
				genIdent(annotatedContextVar),
				genIdent(inboundMarshalerVar),
				genIdent(serverVar),
				genIdent(interceptorVar),
				genIdent(reqVar),
				genIdent(pathParamsVar),
			),
		),
	}
}

func generateField(pointer bool, packageName, selectorName string, names ...string) *ast.Field {
	var (
		fieldType ast.Expr
	)

	nameList := make([]*ast.Ident, len(names))

	for i := range names {
		nameList[i] = genIdent(names[i])
	}
	if packageName == "" {
		fieldType = &ast.Ident{
			Name: selectorName,
		}
	} else {
		fieldType = getSelectorExpr(packageName, selectorName)
	}
	if pointer {
		fieldType = getStarExpr(fieldType)
	}

	return &ast.Field{
		Names: nameList,
		Type:  fieldType,
	}
}

func generateFunctionDeclaration(funcData assignmentWithRPCMethodName, serverType string) *ast.FuncDecl {
	return &ast.FuncDecl{
		Doc:  getEmptyLine(),
		Type: generateFunctionDeclarationType(serverType),
		Name: genIdent(funcData.funcName),
		Body: getFunctionDeclarationBody(funcData),
	}
}

func generateFunctionDeclarationType(serverType string) *ast.FuncType {
	return &ast.FuncType{
		Params: fieldsToList(
			generateField(false, contextPackage, contextSelector, ctxVar, annotatedContextVar),
			generateField(false, runtimePackage, marshalerSelector, inboundMarshalerVar),
			generateField(false, "", serverType, serverVar),
			generateField(true, grpcPackage, unaryServerInterceptorSelector, interceptorVar),
			generateField(true, httpPackage, requestSelector, reqVar),
			&ast.Field{
				Names: identToList(genIdentWithObj(pathParamsVar, ast.Var)),
				Type: &ast.MapType{
					Key:   genIdent(stringType),
					Value: genIdent(stringType),
				},
			},
		),
		Results: fieldsToList(
			generateField(false, runtimePackage, serverMetadataSelector, mdVar),
			generateField(false, protoPackage, messageSelector, respVar),
			generateField(false, "", errType, errVar),
		),
	}
}

func getFunctionDeclarationBody(funcData assignmentWithRPCMethodName) *ast.BlockStmt {
	return getBlockStmnt(
		generateStructDeclaration(),
		generateHandlerAssignment(funcData),
		generateInterfaceDeclaration(),
		generateIfInterceptorIsZeroStmt(funcData),
		getIfStmt(
			getBinaryExpr(token.NEQ, errVar, nilVar),
			nil,
			nil,
			stmtToList(getReturnStmt()),
		),
		&ast.AssignStmt{
			Lhs: exprToList(genIdentWithObj(dataVar, ast.Var), genIdentWithObj(okVar, ast.Var)),
			Tok: token.DEFINE,
			Rhs: exprToList(getTypeAssertExpr(genIdent(handlerResponseItemVar), genIdent(handlerResponseType))),
		},
		getIfStmt(getUnaryExpr(token.NOT, genIdent(okVar)), nil, nil, stmtToList(getReturnStmt())),
		getReturnStmt(getSelectorExpr(dataVar, mdVar), getSelectorExpr(dataVar, respVar), genIdent(nilVar)),
	)
}

func generateIfInterceptorIsZeroStmt(funcData assignmentWithRPCMethodName) *ast.IfStmt {
	return getIfStmt(
		getBinaryExpr(token.EQL, interceptorVar, nilVar),
		nil,
		stmtToList(
			&ast.AssignStmt{
				Lhs: exprToList(genIdent(handlerResponseItemVar), genIdent(errVar)),
				Tok: token.ASSIGN,
				Rhs: exprToList(
					getCallExpr(
						getParenExpr(getStarExpr(genIdent(interceptorVar))),
						genIdent(ctxVar),
						genIdent(reqVar),
						getUnaryExpr(token.AND, getCompositeLit(
							getSelectorExpr(grpcPackage, unaryServerInfoSelector),
							getKeyValExpr(genIdent(serverStructField), genIdent(serverVar)),
							getKeyValExpr(genIdent(fullMethodStructField), getBasicLit(token.STRING, funcData.rpcMethodName)))),
						genIdent(handlerVar),
					),
				),
			},
		),
		stmtToList(
			&ast.AssignStmt{
				Tok: token.ASSIGN,
				Lhs: exprToList(genIdent(handlerResponseItemVar), genIdent(errVar)),
				Rhs: exprToList(
					getCallExpr(
						genIdent(handlerVar),
						genIdent(ctxVar),
						genIdent(reqVar),
					)),
			},
		),
	)
}

func generateHandlerAssignment(funcData assignmentWithRPCMethodName) *ast.AssignStmt {
	return &ast.AssignStmt{
		Tok: token.DEFINE,
		Lhs: exprToList(genIdentWithObj(handlerVar, ast.Var)),
		Rhs: exprToList(
			&ast.FuncLit{
				Type: &ast.FuncType{
					Params: fieldsToList(
						generateField(false, contextPackage, contextSelector, ctxVar),
						getEmptyInterface(reqVar),
					),
					Results: fieldsToList(
						getEmptyInterface(""),
						generateField(false, "", errType),
					),
				},
				Body: getBlockStmnt(
					getIfStmt(
						genIdent(okVar),
						&ast.AssignStmt{
							Tok: token.DEFINE,
							Lhs: exprToList(genIdentWithObj(reqVar, ast.Var), genIdentWithObj(okVar, ast.Var)),
							Rhs: exprToList(
								getTypeAssertExpr(genIdent(reqVar), getStarExpr(getSelectorExpr(httpPackage, requestSelector))),
							),
						},
						nil,
						stmtToList(
							funcData.assignStmt,
							getReturnStmt(
								getCompositeLit(
									genIdent(handlerResponseType),
									getKeyValExpr(genIdent(respVar),
										genIdent(respVar)),
									getKeyValExpr(genIdent(mdVar),
										genIdent(mdVar)),
								),
								genIdent(errVar),
							),
						),
					),
					getReturnStmt(
						genIdent(nilVar),
						getCallExpr(
							getSelectorExpr(fmtPackage, errorfSelector),
							exprToList(
								getBasicLit(
									token.STRING,
									fmt.Sprintf("\"error converting req to *%s.Request\"", httpPackage),
								),
							)...,
						),
					),
				),
			}),
	}
}

func generateStructDeclaration() *ast.DeclStmt {
	return getDeclStmt(
		token.TYPE,
		&ast.TypeSpec{
			Name: genIdentWithObj(handlerResponseType, ast.Typ),
			Type: getStructType(
				generateField(false, runtimePackage, serverMetadataSelector, mdVar),
				generateField(false, protoPackage, messageSelector, respVar),
			),
		},
	)
}

func generateInterfaceDeclaration() *ast.DeclStmt {
	return getDeclStmt(
		token.VAR,
		&ast.ValueSpec{
			Names: identToList(genIdentWithObj(handlerResponseItemVar, ast.Var)),
			Type: &ast.InterfaceType{
				Methods: fieldsToList(),
			},
		},
	)
}

func getDeclStmt(token token.Token, specs ...ast.Spec) *ast.DeclStmt {
	return &ast.DeclStmt{
		Decl: &ast.GenDecl{
			Tok:   token,
			Specs: specs,
		},
	}
}

func getBinaryExpr(op token.Token, x, y string) *ast.BinaryExpr {
	return &ast.BinaryExpr{
		Op: op,
		X:  genIdent(x),
		Y:  genIdent(y),
	}
}

func getIfStmt(cond ast.Expr, init ast.Stmt, elseItem []ast.Stmt, body []ast.Stmt) *ast.IfStmt {
	var elseBlock ast.Stmt
	if len(elseItem) != 0 {
		elseBlock = getBlockStmnt(elseItem...)
	}
	return &ast.IfStmt{
		Cond: cond,
		Init: init,
		Body: getBlockStmnt(body...),
		Else: elseBlock,
	}
}

func getBlockStmnt(in ...ast.Stmt) *ast.BlockStmt {
	return &ast.BlockStmt{
		List: in,
	}
}

func getUnaryExpr(token token.Token, expr ast.Expr) *ast.UnaryExpr {
	return &ast.UnaryExpr{
		Op: token,
		X:  expr,
	}
}

func getTypeAssertExpr(x, typeOf ast.Expr) *ast.TypeAssertExpr {
	return &ast.TypeAssertExpr{
		X:    x,
		Type: typeOf,
	}
}

func getCompositeLit(typeOf ast.Expr, eltItems ...ast.Expr) *ast.CompositeLit {
	return &ast.CompositeLit{
		Type: typeOf,
		Elts: eltItems,
	}
}

func exprToList(expr ...ast.Expr) []ast.Expr {
	return expr
}

func stmtToList(stmt ...ast.Stmt) []ast.Stmt {
	return stmt
}

func getReturnStmt(expr ...ast.Expr) *ast.ReturnStmt {
	if len(expr) == 0 {
		return &ast.ReturnStmt{}
	}
	return &ast.ReturnStmt{
		Results: expr,
	}
}

func getSelectorExpr(x, sel string) *ast.SelectorExpr {
	return &ast.SelectorExpr{
		X:   genIdent(x),
		Sel: genIdent(sel),
	}
}

func getInterceptorField() *ast.Field {
	return &ast.Field{
		Names: identToList(genIdentWithObj(interceptorVar, ast.Var)),
		Type:  getStarExpr(getSelectorExpr(grpcPackage, unaryServerInterceptorSelector)),
	}
}

func getStarExpr(in ast.Expr) *ast.StarExpr {
	return &ast.StarExpr{
		X: in,
	}
}

func getBasicLit(token token.Token, value string) *ast.BasicLit {
	return &ast.BasicLit{
		Kind:  token,
		Value: value,
	}
}

func getKeyValExpr(key, val ast.Expr) *ast.KeyValueExpr {
	return &ast.KeyValueExpr{
		Key:   key,
		Value: val,
	}
}

func fieldsToList(fields ...*ast.Field) *ast.FieldList {
	return &ast.FieldList{
		List: fields,
	}
}

func getParenExpr(expr ast.Expr) *ast.ParenExpr {
	return &ast.ParenExpr{
		X: expr,
	}
}

func getCallExpr(fun ast.Expr, args ...ast.Expr) *ast.CallExpr {
	return &ast.CallExpr{
		Fun:  fun,
		Args: args,
	}
}

func getStructType(fields ...*ast.Field) *ast.StructType {
	return &ast.StructType{
		Fields: fieldsToList(fields...),
	}
}

func getEmptyLine() *ast.CommentGroup {
	return &ast.CommentGroup{
		List: []*ast.Comment{
			{},
		},
	}
}

func getEmptyInterface(name string) *ast.Field {
	return &ast.Field{
		Names: identToList(genIdent(name)),
		Type: &ast.InterfaceType{
			Methods: fieldsToList(),
		},
	}
}

func identToList(idents ...*ast.Ident) []*ast.Ident {
	return idents
}
