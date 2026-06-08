# Contributing to Oracle Stream Studio

Thank you for your interest in contributing to **Oracle Stream Studio**! This guide outlines the development setup, test execution, commit conventions, and submission workflow.

---

## 🛠️ Development Setup

To build and run the project locally, you will need:
- **Go** (version 1.22 or higher)
- **FFmpeg** (installed and available in system `PATH`)
- **ZeroMQ** libraries (development headers for zmq4 bindings)

### 1. Clone the repository
```bash
git clone https://github.com/paraisospelosul/oracle-stream-studio.git
cd oracle-stream-studio
```

### 2. Install dependencies
```bash
go mod download
```

### 3. Run the development server
```bash
go run . --data-dir ./data --port 8080
```

---

## 🧪 Testing

We require all modifications to be covered by unit tests. To run tests:

```bash
# Run all tests
go test -v ./...

# Run tests with coverage profiling
go test -v -coverprofile=coverage.out ./...

# View coverage in your browser
go tool cover -html=coverage.out
```

---

## 📝 Commit Conventions

We follow clean commit message standards to generate accurate changelogs:

- `feat:` for new features (e.g., `feat: add SRT failover strategy`)
- `fix:` for bug fixes (e.g., `fix: resolve race in broadcaster`)
- `security:` for security-related improvements (e.g., `security: implement rate limiting`)
- `test:` for adding or updating unit tests (e.g., `test: add coverage for recorder`)
- `docs:` for documentation updates (e.g., `docs: update setup guide`)

---

## 🚀 Submission Process

1. **Fork the repository** on GitHub.
2. **Create a branch** for your changes (`git checkout -b feature/my-cool-feature`).
3. **Commit your changes** with descriptive and categorized commit messages.
4. **Write unit tests** and verify that they pass.
5. **Format your code** with `gofmt -s -w .` before committing.
6. **Submit a Pull Request** (PR) to the `main` branch.

---

## 🤝 Code of Conduct

Please help maintain a professional, welcoming, and inclusive community environment. Respect all contributors and report any issues to the maintainers listed in `MAINTAINERS.md`.
