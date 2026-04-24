# Idiomatic Go Guide

A distilled guide to writing Go code that reads like Go code. Draws on the official sources at the end.

The meta-principle: **Go values simplicity, explicitness, and predictability over cleverness or density.** Most of the conventions below exist to make code fast to skim for a reader who has never seen it before. When you find yourself reaching for an abstraction or a file split, ask whether it makes the code easier to read for someone else, not whether it feels tidier to you.

## TL;DR

- **Files are not scoping units.** Long files (1,000+ lines) are fine if they're about one thing. Split by concept, not line count.
- **Packages are API boundaries, not folders.** Avoid splitting a package into sub-packages for organization.
- **Accept interfaces, return structs.** Define interfaces in the consuming package, keep them small.
- **Errors are values.** Wrap with `%w`, handle at each level, never swallow.
- **Flat beats nested.** No classes-in-classes, no methods-in-methods. Everything sits at the package level.
- **Zero values should be useful.** Constructors only when you need them.
- **`gofmt` is non-negotiable.** Run it on save.

## Contents

1. [File organization: flat, not nested](#1-file-organization-flat-not-nested)
2. [Conventional file layout](#2-conventional-file-layout)
3. [Package structure](#3-package-structure)
4. [Naming](#4-naming)
5. [Types, pointers, and interfaces](#5-types-pointers-and-interfaces)
6. [Errors](#6-errors)
7. [Nesting is shallow](#7-nesting-is-shallow)
8. [Formatting and tooling](#8-formatting-and-tooling)
9. [Concurrency](#9-concurrency)
10. [A handful of smaller idioms](#10-a-handful-of-smaller-idioms)

---

## 1. File organization: flat, not nested

### Files are not scoping units

Unlike Python (file = module), Java (file = class), or JavaScript (file = module with its own imports), **a Go file is just a file**. The compiler concatenates all `.go` files in a directory and treats them as a single compilation unit. A function in `user.go` can call an unexported helper in `session.go` with no import statement and no ceremony, they share a namespace.

This means splitting a file gives you nothing in Go that you'd get in other languages: no encapsulation, no namespace benefit, no import cleanup. You just get more files to open.

> There is no "one type, one file" convention as in some other languages. As a rule of thumb, files should be focused enough that a maintainer can tell which file contains something, and the files should be small enough that it will be easy to find once there.

- Google's Go Best Practices

### Long files are fine

The standard library routinely ships files of 1,000–6,000 lines. `net/http/server.go` is around 3,500 lines. `runtime/proc.go` is over 6,000. If your file is long because it's about one coherent thing, that's the file doing its job.

What matters is whether a reader can quickly find what they're looking for. A 1,500-line file about one type is easy to navigate. A 400-line file that mixes three unrelated types is not.

### Split by concept, not by line count

Good reasons to split:

- **Different types with independent lifetimes** — `User` and `Session` can share a package but live in `user.go` and `session.go`.
- **Platform-specific code** — `poll_linux.go`, `poll_windows.go`, using build tags.
- **Generated code** — `user_gen.go` or `user.pb.go` separate from hand-written code.
- **Tests** — always `foo_test.go` next to `foo.go` (this is enforced by tooling).

Bad reasons to split:

- "The file got long."
- "This type has a lot of methods."
- "I want to group public and private methods separately."
- "I want to group methods by 'phase' or 'lifecycle'" — usually a smell; the phases almost always belong together.

### Keep a type and its methods together

A struct and all its methods go in one file. Don't spread `User`'s methods across `user_create.go`, `user_validate.go`, `user_serialize.go`. The reader opening `user.go` should see everything `User` is and does in one place.

### Helper types used in one function stay with that function

If you have an unexported type that exists only to support one function — built there, consumed there, never returned or passed elsewhere — it's a local helper, not an independent concept. Keep it in the same file as its consumer.

---

## 2. Conventional file layout

Go files tend to follow a predictable top-to-bottom structure. This isn't enforced, but it's what readers expect:

```go
// Package user manages user accounts and authentication.
package user

import (
    // standard library
    "errors"
    "fmt"
    "time"

    // external deps
    "github.com/google/uuid"

    // internal
    "github.com/myorg/myapp/internal/db"
)

// 1. Constants
const (
    maxNameLength = 64
    defaultRole   = "member"
)

// 2. Package-level variables (including sentinel errors)
var (
    ErrNotFound = errors.New("user not found")
    ErrInvalid  = errors.New("invalid user data")
)

// 3. The main type(s)
type User struct {
    // Exported fields first
    ID      string
    Name    string
    Email   string

    // Unexported fields
    created time.Time
}

// 4. Constructors — grouped with the type
func New(name string) *User {
    return &User{
        ID:      uuid.NewString(),
        Name:    name,
        created: time.Now(),
    }
}

func FromJSON(data []byte) (*User, error) { /* ... */ }

// 5. Methods on the type
func (u *User) Rename(name string) error { /* ... */ }
func (u *User) Promote()                  { /* ... */ }
func (u *User) validate() error           { /* ... */ }  // unexported last

// 6. Package-level functions
func Compare(a, b *User) int { /* ... */ }

// 7. Unexported helpers
func normalizeName(s string) string { /* ... */ }
```

The principle: **a reader scanning top-to-bottom should be able to learn what's in the file without jumping around.** Constants and vars set context. Types come before the functions that use them. Constructors come before methods. Helpers go at the bottom like footnotes.

### Group by type, not by kind

If a file has two types, put each type with _its own_ constructors and methods. Don't put all types at the top and all methods at the bottom.

```go
// Good
type User struct { ... }
func NewUser(...) *User         { ... }
func (u *User) Rename(...) error { ... }

type Session struct { ... }
func NewSession(...) *Session       { ... }
func (s *Session) Expire()           { ... }

// Bad — forces the reader to scroll back and forth
type User struct { ... }
type Session struct { ... }
func NewUser(...) *User          { ... }
func NewSession(...) *Session    { ... }
func (u *User) Rename(...) error { ... }
func (s *Session) Expire()        { ... }
```

---

## 3. Package structure

### Packages are API boundaries, not folders

A package is a semantic unit. Everything inside it shares unexported access and presents one unified API to callers. **Resist the urge to split a package into sub-packages for organizational reasons.** It fragments the API, forces you to export things that should stay private, and creates import-cycle problems.

### Typical project layout

```
myapp/
  go.mod
  main.go                       // or cmd/myapp/main.go for multiple binaries

  cmd/
    myapp/
      main.go                   // tiny — parses flags, wires dependencies

  internal/                     // private to this module (language-enforced)
    auth/
      auth.go
      token.go
      middleware.go
    billing/
      billing.go
      invoice.go
    storage/
      storage.go
      postgres.go
```

- `cmd/` holds `main` packages, one per binary.
- `internal/` is a Go language feature: anything under `internal/` can only be imported by code rooted at the parent directory. Use it generously.
- Flat is fine for small projects. Don't invent structure you don't need.

### Package names

From _Effective Go_: short, lowercase, single-word, no underscores or mixedCaps.

- Good: `user`, `auth`, `http`, `tabwriter`
- Bad: `userManagement`, `auth_utils`, `common`, `utils`, `helpers`

Avoid "junk drawer" packages (`utils`, `common`, `helpers`). They accumulate unrelated code and become a maintenance hazard.

The package name should be the same as its directory name, and it should describe what the package _is_, not how it's organized.

---

## 4. Naming

- **Exported** identifiers start with an uppercase letter (`User`, `NewClient`); **unexported** ones start lowercase (`parseHeader`, `userStore`).
- **Acronyms stay uppercase**: `HTTPServer`, not `HttpServer`; `userID`, not `userId`.
- **Short names in short scopes, longer in wider ones**: `i` for a loop index, `ctx` for a `context.Context`, a descriptive name for a package-level variable.

### Avoid stutter

Don't repeat the package name in a type:

```go
// In package user
type User struct { ... }       // Good: user.User is fine
type UserRecord struct { ... } // Bad: user.UserRecord stutters
```

Don't repeat the type name in a method:

```go
func (u *User) Name() string       // Good: u.Name()
func (u *User) GetUserName() string // Bad: u.GetUserName() stutters + "Get" prefix
```

### No "Get" prefix on getters

Idiomatic Go uses the field name directly. If `User` has an unexported `name` field, the getter is `Name()`, not `GetName()`. Setters do keep `Set`: `SetName()`.

### Receiver names

Short and consistent across a type's methods. One or two letters, derived from the type name:

```go
func (u *User) Rename(...) error { ... }
func (u *User) Promote()         { ... }  // same 'u' every time
```

Don't use `self` or `this`. Don't vary the name between methods.

---

## 5. Types, pointers, and interfaces

### Accept interfaces, return structs

Functions take the smallest interface they need and return concrete types.

```go
// Good: accepts anything readable, returns a concrete type
func countLines(r io.Reader) (int, error) { ... }

func NewClient(cfg Config) *Client { ... }

// Bad: returns an interface, forcing callers into whatever API you designed
func NewClient(cfg Config) ClientInterface { ... }
```

Why: interfaces on input give callers flexibility; concrete returns give callers full access to the type's API and let you add methods later without breaking anyone.

### Interfaces are defined by consumers

Define the interface in the package that _uses_ it, not the package that _implements_ it. Interfaces are usually small — one or two methods — and named for what they do (`Reader`, `Stringer`, `Closer`).

```go
// In the package that consumes the dependency:
type userStore interface {
    GetUser(id string) (*User, error)
}

func greet(s userStore, id string) (string, error) { ... }
```

The concrete `*sql.DB`-backed implementation lives elsewhere and doesn't need to know this interface exists. Go's structural typing means it satisfies the interface automatically.

### Zero values should be useful

Well-designed Go types work without explicit initialization:

```go
var buf bytes.Buffer   // ready to use, no constructor needed
buf.WriteString("hi")

var mu sync.Mutex      // zero value is an unlocked mutex
mu.Lock()
```

If users _must_ call `NewThing()` to get a working value, have a real reason.

### Pointer vs. value returns

- Returning `*Config` avoids copying, supports `nil` for "no result," and is the convention for constructor-style functions (`New`, `Open`, `Parse`).
- Returning `Config` is fine for small value types (`time.Time`, `image.Point`) that are naturally copied.

When in doubt for struct-shaped things, return `*T`.

### Composition over inheritance

Go has no classes and no inheritance. Use struct embedding to reuse behavior:

```go
type Logger struct {
    prefix string
}
func (l *Logger) Log(msg string) { fmt.Println(l.prefix, msg) }

type Server struct {
    *Logger      // embedded; Server now has .Log()
    addr string
}
```

This is an "has-a" relationship, not "is-a." It's additive and local.

---

## 6. Errors

### Errors are values

Every function that can fail returns an `error` as the last return value. Callers handle it immediately. No exceptions, no hidden control flow.

```go
data, err := os.ReadFile(path)
if err != nil {
    return nil, fmt.Errorf("read config %s: %w", path, err)
}
```

### Wrap with `%w`, not `%v`

`fmt.Errorf` with `%w` preserves the original error in a chain, so callers can inspect it with `errors.Is` and `errors.As`:

```go
// %w wraps — the original error is recoverable
return fmt.Errorf("parse config: %w", err)

// %v just formats the message — the original error is lost
return fmt.Errorf("parse config: %v", err)
```

Add context at each level ("read config", "parse config") so a failure tells a story from where it happened up to where it was handled.

### Sentinel errors

Define expected error values at the package level, named `ErrX`:

```go
var ErrNotFound = errors.New("user not found")

// Callers check with errors.Is, which walks the wrap chain
if errors.Is(err, user.ErrNotFound) { ... }
```

### Don't swallow errors

Never assign to `_` to make the compiler happy. Either handle the error, return it, or log it with context.

---

## 7. Nesting is shallow

Go deliberately avoids deep nesting. A Go file has effectively two levels:

1. **Package level** (indentation 0): all types, all functions, all variables, all constants.
2. **Function body level**: local variables, loops, conditionals, short anonymous functions.

There are no classes-inside-classes, no methods-inside-methods, no module-level closures for private state. Every symbol in a file sits at column 0, findable by simple text search. This is a big reason long Go files stay readable — the structure is wide and flat, not tall and deep.

Anonymous functions exist (`go func() { ... }()`, short callbacks, `defer`), but they're used sparingly and kept short. If a closure is getting long, it becomes a top-level function.

---

## 8. Formatting and tooling

`goimports` is a superset of `gofmt` — it formats code and manages import blocks (grouping, sorting, removing unused). `golangci-lint` runs a battery of static checks beyond formatting.

```bash
# format all files (fixes imports + gofmt in one pass)
goimports -w .

# lint
golangci-lint run ./...
```

There is no pre-commit hook or CI lint step today. Run both locally before pushing.

---

## 9. Concurrency

### Communicate via channels; synchronize with mutexes where simpler

Go's motto: _"Don't communicate by sharing memory; share memory by communicating."_ Channels are for passing ownership of data between goroutines. But for simple shared state (a counter, a cache), a `sync.Mutex` is often clearer than channels. Use the right tool.

### Pass `context.Context` as the first parameter

Any function doing I/O, waiting, or potentially long work should accept a `context.Context` as its first parameter, conventionally named `ctx`:

```go
func FetchUser(ctx context.Context, id string) (*User, error) { ... }
```

This lets callers cancel work, set deadlines, and pass request-scoped values.

---

## 10. A handful of smaller idioms

- **Defer cleanup immediately after acquiring a resource.** `f, err := os.Open(path); if err != nil { ... }; defer f.Close()`. Keeps acquisition and release visually paired.
- **Empty struct for "set" types.** `map[string]struct{}` — the `struct{}` takes zero bytes and signals "I only care about the keys."
- **Return early on errors.** Don't nest happy paths inside `if err == nil`. Handle the error, return, and let the rest of the function be unindented.
- **`//` comments, not `/* */`.** Block comments are reserved for package-level documentation.
- **Doc comments are complete sentences.** `// User represents an authenticated account.` — starts with the identifier name, ends with a period.

---

## Sources

- [Effective Go](https://go.dev/doc/effective_go) — the official, canonical guide. Read this first if you read nothing else.
- [Go Style Guide (Google)](https://google.github.io/styleguide/go/) — normative style guide used at Google; good for specific decisions.
- [Go Style Decisions (Google)](https://google.github.io/styleguide/go/decisions.html) — covers detailed questions like naming, formatting, and package structure.
- [Go Style Best Practices (Google)](https://google.github.io/styleguide/go/best-practices.html) — includes the file-size guidance quoted above.
- [Organizing a Go module](https://go.dev/doc/modules/layout) — official guide to project layout.
- [Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md) — well-regarded industry guide, especially strong on concurrency patterns.
- [The standard library source](https://github.com/golang/go/tree/master/src) — the ultimate reference. When in doubt, find a comparable package and copy its shape. `net/http`, `io`, `encoding/json`, and `os` are particularly instructive.
