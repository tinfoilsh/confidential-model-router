# Go standards

Distilled from sources at the end.

## TL;DR

- **Files are not scoping units.** Long files (1,000+ lines) are fine if they're about one thing. Split by concept, not line count.
- **Packages are API boundaries, not folders.** Avoid splitting a package into sub-packages for organization.
- **Generally accept interfaces, return concrete types.** Define interfaces in the consuming package, keep them small. Returning an interface is fine for encapsulation, factories, or when the interface is the product (`io.Writer`, `hash.Hash`).
- **Return errors, handle them at each call site.** Wrap with `%w` when callers need to inspect; use `%v` at system boundaries. Add non-redundant context.
- **Prefer flat structure.** No classes-in-classes, no methods-in-methods. Everything sits at the package level.
- **Zero values should be useful.** Constructors only when you need them.

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

This means splitting a file gives you fewer of the benefits you'd get in other languages: no encapsulation, no namespace benefit, no import cleanup. But splitting can still help navigability.

> There is no "one type, one file" convention as in some other languages. As a rule of thumb, files should be focused enough that a maintainer can tell which file contains something, and the files should be small enough that it will be easy to find once there.

- Google's Go Best Practices

### Long files can be fine

The standard library routinely ships files of several thousand lines. `net/http/server.go` is over 4,000 lines. `runtime/proc.go` is over 8,000. If your file is long because it's about one coherent thing, that's the file doing its job. That said, it's "usually not a good idea to have a single file with many thousands of lines in it, or having many tiny files" (Google's Best Practices).

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

## 2. File layout

**Team convention** — the official sources don't prescribe a within-file ordering, but Daniel likes this predictable top-to-bottom structure:

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

The principle: **a reader scanning top-to-bottom should be able to learn what's in the file without jumping around.** Types come before the functions that use them. Constructors come before methods. Helpers go at the bottom.

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

A package is a semantic unit. Everything inside it shares unexported access and presents one unified API to callers. Think carefully before splitting a package into sub-packages for purely organizational reasons — it can fragment the API, force you to export things that should stay private, and create import-cycle problems. That said, when something is conceptually distinct, giving it its own small package can make it easier to use.

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
- **Acronyms stay uppercase**: `HTTPServer`, not `HttpServer`; `userID`, not `userId`. Exception: initialisms that contain lowercase letters in standard prose keep their conventional form (`gRPC`, `iOS`, `DDoS`).
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

### No "Get" prefix on simple getters

Idiomatic Go uses the field name directly. If `User` has an unexported `name` field, the getter is `Name()`, not `GetName()`. Setters do keep `Set`: `SetName()`.

Exception: `Get` is appropriate when the underlying concept genuinely uses the word "get" (e.g., HTTP GET semantics: `http.Get`, `client.GetObject`).

### Receiver names

Short and consistent across a type's methods. One or two letters, derived from the type name:

```go
func (u *User) Rename(...) error { ... }
func (u *User) Promote()         { ... }  // same 'u' every time
```

Don't use `self` or `this`. Don't vary the name between methods.

---

## 5. Types, pointers, and interfaces

### Generally accept interfaces, return concrete types

Functions take the smallest interface they need and generally return concrete types.

```go
// Good: accepts anything readable, returns a concrete type
func countLines(r io.Reader) (int, error) { ... }

func NewClient(cfg Config) *Client { ... }
```

Why: interfaces on input give callers flexibility; concrete returns give callers full access to the type's API and let you add methods later without breaking anyone.

Returning an interface is appropriate for encapsulation (the `error` interface itself), factory/strategy patterns, and when the interface is the product (e.g., `crc32.NewIEEE` returns `hash.Hash32`). The official sources treat this as a guideline, not a strict rule.

### Interfaces are generally defined by consumers

Define the interface in the package that _uses_ it, not the package that _implements_ it. Interfaces are usually small — one or two methods — and named for what they do (`Reader`, `Stringer`, `Closer`).

Exceptions: the producer should export an interface when the interface _is_ the product (`io.Writer`, `hash.Hash`), when many consumers would otherwise mirror the same interface, or to break circular dependencies.

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

### Return errors, handle them at each call site

Every function that can fail returns an `error` as the last return value. Callers handle it immediately.

```go
data, err := os.ReadFile(path)
if err != nil {
    return nil, fmt.Errorf("read config %s: %w", path, err)
}
```

### `%w` vs `%v` — choose deliberately

`fmt.Errorf` with `%w` preserves the original error in a chain, so callers can inspect it with `errors.Is` and `errors.As`. Use `%w` when the wrapped error is part of your package's documented contract and callers will programmatically inspect it.

Use `%v` (or `errors.New`) when you're producing a fresh error at a system boundary, or when the underlying error is an implementation detail callers shouldn't depend on.

```go
// %w — callers can use errors.Is/errors.As to inspect
return fmt.Errorf("parse config: %w", err)

// %v — creates an opaque error; appropriate at API/system boundaries
return fmt.Errorf("parse config: %v", err)
```

Add context when you have _non-redundant_ information to contribute. Don't wrap purely to indicate failure — if the wrapping adds no information beyond "this failed," return `err` directly.

### Sentinel errors

Define expected error values at the package level, named `ErrX`:

```go
var ErrNotFound = errors.New("user not found")

// Callers check with errors.Is, which walks the wrap chain
if errors.Is(err, user.ErrNotFound) { ... }
```

### Don't swallow errors

Avoid assigning errors to `_` to make the compiler happy. Either handle the error, return it, or log it with context. In the rare case where ignoring an error is safe (e.g., `(*bytes.Buffer).Write` which is documented to never fail), add a comment explaining why.

---

## 7. Nesting is shallow

Go deliberately avoids deep nesting. A Go file has effectively two levels:

1. **Package level** (indentation 0): all types, all functions, all variables, all constants.
2. **Function body level**: local variables, loops, conditionals, short anonymous functions.

There are no classes-inside-classes, no methods-inside-methods, no module-level closures for private state. Every symbol in a file sits at column 0, findable by simple text search. This is a big reason long Go files stay readable.

Anonymous functions / closures are idiomatic and widely used — `defer func() {…}()`, goroutine launches, `sync.Once`, `http.HandlerFunc` adapters, functional options, and table-test setup all rely on them. Keep closures focused; if one is getting long or complex, extract it to a named top-level function.

---

## 8. Formatting and tooling

`goimports` is a superset of `gofmt` — it formats code and manages import blocks (grouping, sorting, removing unused). `golangci-lint` runs a battery of static checks beyond formatting.

```bash
# format all files (fixes imports + gofmt in one pass)
goimports -w .

# lint
golangci-lint run ./...
```

Run both locally before pushing.

---

## 9. Concurrency

### Communicate via channels; synchronize with mutexes where simpler

Go's motto: _"Don't communicate by sharing memory; share memory by communicating."_ Channels are for passing ownership of data between goroutines. But for simple shared state (a counter, a cache), a `sync.Mutex` is often clearer than channels.

### Pass `context.Context` as the first parameter

Any function doing I/O, waiting, or potentially long work should accept a `context.Context` as its first parameter, conventionally named `ctx`:

```go
func FetchUser(ctx context.Context, id string) (*User, error) { ... }
```

This lets callers cancel work, set deadlines, and pass request-scoped values. Exceptions exist where `ctx` comes from elsewhere: HTTP handlers get it from `req.Context()`, and test functions from `(testing.TB).Context()`.

### Goroutine lifetimes

When you spawn goroutines, make it clear when — or whether — they exit. Goroutines can leak by blocking on channel sends or receives, and leaked goroutines are a common source of production bugs.

- Never start a goroutine without knowing how it will stop.
- The caller that launches a goroutine should own its shutdown — typically via a `context.Context` cancellation, a done channel, or a `sync.WaitGroup`.
- Prefer synchronous functions over asynchronous ones. Let the _caller_ decide whether to `go` your function rather than baking concurrency inside it. This keeps goroutine ownership local and makes leaks easier to prevent.

### Generics

Generics (Go 1.18+) are allowed where they fulfill your requirements. In many cases, a conventional approach using slices, maps, interfaces, and type assertions works just as well without the added complexity.

- Don't use generics just because an algorithm doesn't care about member element types — consider whether a concrete type or interface would be simpler.
- Don't use generics to invent domain-specific languages or overly abstract frameworks.
- "Write code, don't design types" — start concrete, generalize only when you have real duplication.

---

## 10. A handful of smaller idioms

- **Defer cleanup immediately after acquiring a resource.** `f, err := os.Open(path); if err != nil { ... }; defer f.Close()`. Keeps acquisition and release visually paired.
- **Sets via map.** `map[string]bool` is the simple, idiomatic choice (per Google's canonical Style Guide). `map[string]struct{}` is a common variation that uses zero bytes per entry — either is fine.
- **Return early on errors.** Don't nest happy paths inside `if err == nil`. Handle the error, return, and let the rest of the function be unindented.
- **`//` comments are the norm.** Block comments (`/* */`) are fine for package-level documentation, inline expressions, or temporarily disabling large blocks of code.
- **Doc comments are complete sentences.** `// User represents an authenticated account.` — starts with the identifier name, ends with a period.

---

## Sources

- [Go Style Guide (Google)](https://google.github.io/styleguide/go/) — normative and canonical style guide used at Google
- [Go Style Decisions (Google)](https://google.github.io/styleguide/go/decisions.html) — normative but not canonical; covers naming, formatting, and package structure
- [Go Style Best Practices (Google)](https://google.github.io/styleguide/go/best-practices.html) — neither normative nor canonical; auxiliary guidance that may not apply in every circumstance
- [Organizing a Go module](https://go.dev/doc/modules/layout) — official guide to project layout
- [The standard library source](https://github.com/golang/go/tree/master/src) — the ultimate reference. When in doubt, find a comparable package. `net/http`, `io`, `encoding/json`, and `os` are particularly instructive.
- [Effective Go](https://go.dev/doc/effective_go) — Official, canonical guide (note: last significantly updated ~2009; does not cover generics or modules)
