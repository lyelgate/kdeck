# Install kdeck on Windows, checksum-verified.
#   irm https://raw.githubusercontent.com/lyelgate/kdeck/main/install.ps1 | iex
# Options: $env:KDECK_VERSION = "v0.1.0"; $env:KDECK_REPO = "owner/name"
$ErrorActionPreference = "Stop"
$repo = if ($env:KDECK_REPO) { $env:KDECK_REPO } else { "lyelgate/kdeck" }
$arch = switch ($env:PROCESSOR_ARCHITECTURE) { "ARM64" { "arm64" } default { "amd64" } }
$version = if ($env:KDECK_VERSION) { $env:KDECK_VERSION } else { (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name }
$file = "kdeck_$($version.TrimStart('v'))_windows_$arch.zip"
$base = "https://github.com/$repo/releases/download/$version"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("kdeck-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading $file ($version)"
    Invoke-WebRequest "$base/$file" -OutFile "$tmp\$file"
    Invoke-WebRequest "$base/checksums.txt" -OutFile "$tmp\checksums.txt"
    $want = (Select-String -Path "$tmp\checksums.txt" -Pattern ([Regex]::Escape($file) + '$')).Line.Split(" ")[0]
    $got = (Get-FileHash "$tmp\$file" -Algorithm SHA256).Hash.ToLower()
    if (-not $want -or $want -ne $got) { throw "checksum mismatch for $file; not installing" }
    $dir = Join-Path $env:LOCALAPPDATA "kdeck\bin"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Expand-Archive "$tmp\$file" -DestinationPath $tmp -Force
    Copy-Item "$tmp\kdeck.exe" "$dir\kdeck.exe" -Force
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($userPath -notlike "*$dir*") {
        [Environment]::SetEnvironmentVariable("Path", "$dir;$userPath", "User")
        Write-Host "Added $dir to your PATH (open a new terminal)."
    }
    & "$dir\kdeck.exe" about
    Write-Host "Next: kdeck setup"
} finally {
    Remove-Item -Recurse -Force $tmp
}
