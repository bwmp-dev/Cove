param([switch]$EndToEnd)
$ErrorActionPreference = 'Stop'
$testName = 'cove-test-db-' + [Guid]::NewGuid().ToString('N')
$prior = @{}
foreach ($key in @('POSTGRES_PASSWORD', 'COVE_TEST_DATABASE_URL', 'COVE_TEST_DATABASE_CONTAINER', 'COVE_E2E', 'GOOS', 'GOARCH')) {
    $prior[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
}
$created = $false
try {
    $random = New-Object byte[] 24
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    $rng.GetBytes($random)
    $rng.Dispose()
    $env:POSTGRES_PASSWORD = ([BitConverter]::ToString($random)).Replace('-', '')
    docker run -d --name $testName --label cove.test=true -e POSTGRES_PASSWORD -e POSTGRES_DB=cove_test -p 127.0.0.1::5432 postgres:17-alpine
    if ($LASTEXITCODE -ne 0) { throw 'Could not create isolated test database.' }
    $created = $true
    $env:COVE_TEST_DATABASE_CONTAINER = $testName
    $published = docker port $testName 5432/tcp
    if ($LASTEXITCODE -ne 0 -or !$published) { throw 'Test database port unavailable.' }
    $testPort = ($published -split ':')[-1]
    $env:COVE_TEST_DATABASE_URL = "postgres://postgres:$($env:POSTGRES_PASSWORD)@127.0.0.1:${testPort}/cove_test?sslmode=disable"
    $ready = $false
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        docker exec $testName pg_isready -h 127.0.0.1 -U postgres
        if ($LASTEXITCODE -eq 0) { $ready = $true; break }
        Start-Sleep -Seconds 1
    }
    if (!$ready) { throw 'Test database did not become ready.' }
    if ($EndToEnd) {
        $env:GOOS = 'linux'
        $env:GOARCH = 'amd64'
        go build -o bin/cove-worker-linux-amd64 ./cmd/cove-worker
        if ($LASTEXITCODE -ne 0) { throw 'Worker build failed.' }
        [Environment]::SetEnvironmentVariable('GOOS', $prior['GOOS'], 'Process')
        [Environment]::SetEnvironmentVariable('GOARCH', $prior['GOARCH'], 'Process')
        $env:COVE_E2E = '1'
    }
    go test -v ./... -timeout 10m
    if ($LASTEXITCODE -ne 0) { throw 'Tests failed.' }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go vet failed.' }
} finally {
    if ($created) { docker rm -f -v $testName }
    foreach ($key in $prior.Keys) { [Environment]::SetEnvironmentVariable($key, $prior[$key], 'Process') }
}
