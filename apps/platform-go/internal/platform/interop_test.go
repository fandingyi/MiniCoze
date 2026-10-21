package platform

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestNestJSJWTAndBcryptInterop(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node required for NestJS interop")
	}
	auth, store := setup(t)
	goToken, err := auth.Sign(store.user)
	if err != nil {
		t.Fatal(err)
	}
	script := `const fs=require('fs');
 const {JwtService}=require('../../../backend/node_modules/@nestjs/jwt');
 const jwt=new JwtService({secret:'test-secret-not-for-production',signOptions:{algorithm:'HS256',expiresIn:'2h'}});
 const bcrypt=require('../../../backend/node_modules/bcrypt');
 const token=fs.readFileSync(0,'utf8');
 const user=jwt.verify(token,{algorithms:['HS256']});
 if(user.sub!=='u1')throw Error('Go subject mismatch');
 process.stdout.write(JSON.stringify({token:jwt.sign({sub:'u1',email:'user@example.com',username:'user'}),hash:bcrypt.hashSync('password123',10)}));`
	cmd := exec.Command("node", "-e", script)
	cmd.Stdin = strings.NewReader(goToken)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Node interop failed: %v %s", err, output)
	}
	var fixture struct{ Token, Hash string }
	if err = json.Unmarshal(output, &fixture); err != nil {
		t.Fatal(err)
	}
	claims, err := auth.Verify(fixture.Token)
	if err != nil || claims.Subject != "u1" {
		t.Fatalf("NestJS token rejected: %v", err)
	}
	if err = bcrypt.CompareHashAndPassword([]byte(fixture.Hash), []byte("password123")); err != nil {
		t.Fatalf("NestJS password incompatible: %v", err)
	}
}
