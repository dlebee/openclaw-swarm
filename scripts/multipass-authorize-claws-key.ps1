#!/usr/bin/env pwsh
# Adds the active claws SSH public key to a Multipass VM user's authorized_keys.

$vmName   = Read-Host "Multipass VM name"
$vmUser   = Read-Host "SSH user on the VM [default: ubuntu]"
if (-not $vmUser) { $vmUser = "ubuntu" }

$listOutput = go run ./cmd/cli auth list 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-Error "claws auth list failed: $listOutput"
    exit 1
}

$activeLine = $listOutput |
    Select-Object -Skip 1 |
    Where-Object { $_ -match '^\*' } |
    Select-Object -First 1

if (-not $activeLine) {
    Write-Error "No active claws SSH identity found. Run: claws auth generate <name> && claws auth use <name>"
    exit 1
}

$parts      = $activeLine -split '\s+', 4
$privKeyPath = (Resolve-Path $parts[2].Trim()).Path
$pubKeyPath  = (Resolve-Path $parts[3].Trim()).Path

if (-not (Test-Path $pubKeyPath)) {
    Write-Error "Public key not found at: $pubKeyPath"
    exit 1
}
if (-not (Test-Path $privKeyPath)) {
    Write-Error "Private key not found at: $privKeyPath"
    exit 1
}

$pubKey  = (Get-Content $pubKeyPath -Raw).Trim()
$homeDir = if ($vmUser -eq "root") { "/root" } else { "/home/$vmUser" }

Write-Host "Adding key from $pubKeyPath to $vmUser@$vmName ..."

$script = @"
mkdir -p $homeDir/.ssh
chmod 700 $homeDir/.ssh
grep -qxF '$pubKey' $homeDir/.ssh/authorized_keys 2>/dev/null || echo '$pubKey' >> $homeDir/.ssh/authorized_keys
chmod 600 $homeDir/.ssh/authorized_keys
"@ -replace "`r", ""

multipass exec $vmName -- bash -c $script
if ($LASTEXITCODE -ne 0) {
    Write-Error "Failed to authorize key on $vmName"
    exit 1
}

# Resolve the VM's IPv4 so we can test a real SSH connection (not via multipass exec).
$infoJson = multipass info $vmName --format json | ConvertFrom-Json
$vmIP = $infoJson.info.PSObject.Properties[$vmName].Value.ipv4[0]
if (-not $vmIP) {
    Write-Warning "Could not resolve IP for $vmName - skipping SSH test."
} else {
    Write-Host "Testing SSH connection to $vmUser@$vmIP using private key ..."
    $sshArgs = @("-i", $privKeyPath, "-o", "StrictHostKeyChecking=no", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "$vmUser@$vmIP", "whoami")
    $result = ssh @sshArgs 2>&1
    if ($LASTEXITCODE -eq 0 -and "$result".Trim() -eq $vmUser) {
        Write-Host "SSH test passed - logged in as '$result'."
    } elseif ($LASTEXITCODE -eq 0) {
        Write-Warning "SSH test connected but whoami returned '$result' (expected '$vmUser')."
    } else {
        Write-Error "SSH test failed: $result"
        exit 1
    }
}

Write-Host "Done. claws can now SSH into $vmName as $vmUser."
