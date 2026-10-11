# Fan-out: one fresh `claude -p` per package, driven by THIS script (not by a
# model). Same pattern as Claude Code's best-practices "Fan out across files":
#
#   foreach ($item in queue) { claude -p "<prompt>" --allowedTools ... --permission-mode dontAsk }
#
# gocov does the deterministic parts (queue, prompt, verification, revert, goal
# check); this loop does the orchestration and money control.
#
# Prerequisite: you already ran, and reviewed the output of,
#   gocov report -config CFG   and   gocov plan -config CFG
# and `claude -p` can authenticate (standalone CLI logged in, or ANTHROPIC_API_KEY).
#
# Example:
#   .\fanout.ps1 -Config C:\work\cfg.json -Repo shopapi -Dir C:\work\shopapi -Pilot 3
param(
    [Parameter(Mandatory)][string]$Config,   # gocov config
    [Parameter(Mandatory)][string]$Repo,     # repo name from the config
    [Parameter(Mandatory)][string]$Dir,      # repo directory, where claude runs
    [int]$Pilot = 0,                         # only the first N queued items
    [string]$Model = "sonnet",               # attempt 1
    [string]$Escalate = "opus",              # later attempts
    [int]$Attempts = 2,
    [double]$ItemUsd = 1.50,                 # per-call spend cap
    [double]$TotalUsd = 10,                  # total spend cap for this run
    [switch]$Review,                         # run `gocov review -headless` at the end
    [switch]$DryRun                          # print the claude command and prompt for the first item; call nothing
)
$ErrorActionPreference = "Stop"

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$gocov = Join-Path $here "bin\gocov.exe"
if (-not (Test-Path $gocov)) {
    Write-Host "building gocov..."
    Push-Location (Join-Path $here "gocov"); try { & go build -o ..\bin\gocov.exe . } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { exit 1 }
}
function G([string]$cmd, [string[]]$more = @()) {
    & $gocov $cmd -config $Config -repo $Repo @more
}

# Same permission pattern as the article: pre-approve exactly what is needed and
# deny everything else (dontAsk), because nobody is watching.
$tools = 'Read,Edit,Write,Glob,Grep,Bash(go test *),Bash(go vet *),Bash(go build *),Bash(go list *),Bash(gofmt *),Bash(git diff *),Bash(git status *)'

function Invoke-Claude([string]$model, [string]$inputFile) {
    Push-Location $Dir
    try {
        $json = Get-Content -Raw $inputFile | & claude -p --output-format json --model $model `
            --permission-mode dontAsk --max-budget-usd $ItemUsd --no-session-persistence --allowedTools $tools
        return ($json -join "`n")
    } finally { Pop-Location }
}

if (-not $DryRun) {
    & $gocov preflight -config $Config      # fail in seconds if claude -p cannot authenticate
    if ($LASTEXITCODE -ne 0) { exit 1 }
}

$work = New-Item -ItemType Directory -Path (Join-Path ([IO.Path]::GetTempPath()) ("fanout-" + [guid]::NewGuid()))
$spent = 0.0; $done = 0; $blocked = 0; $n = 0
try {
    $ids = @(G "queue")
    foreach ($id in $ids) {
        G "goal" | Out-Null
        if ($LASTEXITCODE -eq 0) { Write-Host "goal reached by estimate - skipping the remaining items"; break }
        if ($Pilot -gt 0 -and $n -ge $Pilot) { Write-Host "pilot of $Pilot item(s) done"; break }
        if ($spent -gt $TotalUsd) { Write-Host ("total budget `${0} reached (spent `${1:N2}) - stopping; re-run to resume" -f $TotalUsd, $spent); break }
        $n++

        $promptFile = (G "begin" @("-item", $id, "-q") | Select-Object -Last 1)
        if ($LASTEXITCODE -ne 0) { Write-Host "begin failed for $id"; continue }
        $feedbackFile = $promptFile -replace '\.prompt\.md$', '.feedback.md'

        if ($DryRun) {
            Write-Host "--- DRY RUN: $id"
            Write-Host "claude -p --output-format json --model $Model --permission-mode dontAsk --max-budget-usd $ItemUsd --no-session-persistence --allowedTools `"$tools`" < $promptFile"
            Write-Host ""; Get-Content $promptFile; exit 0
        }

        $ok = $false
        for ($att = 1; $att -le $Attempts; $att++) {
            $m = if ($att -gt 1) { $Escalate } else { $Model }
            $in = Join-Path $work "in.md"
            Copy-Item $promptFile $in -Force
            if ((Test-Path $feedbackFile) -and (Get-Item $feedbackFile).Length -gt 0) {
                Add-Content $in "`n`nPREVIOUS ATTEMPT FEEDBACK - fix this first:`n$(Get-Content -Raw $feedbackFile)"
            }
            Write-Host "[$Repo] $id attempt $att/$Attempts (model $m)"
            $raw = Invoke-Claude $m $in
            $res = $null; try { $res = $raw | ConvertFrom-Json } catch {}
            $cost = if ($res -and $res.total_cost_usd) { [double]$res.total_cost_usd } else { 0.0 }
            $spent += $cost
            if ($res -and $res.is_error -and $cost -eq 0) {
                Write-Host "claude -p could not run: $($res.result)"
                Write-Host "(no model call was made; check login / API key, then re-run - the item stays queued)"; exit 1
            }
            G "finish" @("-item", $id, "-cost", $cost.ToString([Globalization.CultureInfo]::InvariantCulture))
            $rc = $LASTEXITCODE
            if ($rc -eq 0) { $ok = $true; $done++; break }
            if ($rc -ne 3) { Write-Host "gocov finish failed ($rc)"; exit $rc }
        }
        if (-not $ok) { G "finish" @("-item", $id, "-abandon"); $blocked++ }
        Write-Host ("[{0}] spent so far: `${1:N2}" -f $Repo, $spent)
    }
    Write-Host ""; Write-Host ("== {0} done, {1} blocked, `${2:N2} spent. Verifying with a fresh full run..." -f $done, $blocked, $spent)
    G "verify"
    if ($Review) { G "review" @("-headless") } else { Write-Host "Next: gocov review (adversarial review of the new tests)" }
} finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
