# Fan-out: one fresh `claude -p` per package, driven by THIS script (not by a
# model). Same pattern as Claude Code's best-practices "Fan out across files":
#
#   foreach ($item in queue) { claude -p "<prompt>" --allowedTools ... --permission-mode dontAsk }
#
# gocov does the deterministic parts (queue, prompt, model and spend cap per
# call, verification, revert, goal check); this loop only orchestrates. Models,
# caps, attempts and timeouts come from the config (models.*, budget.*,
# max_attempts) through `gocov next-call`, by the same rules as `gocov run`.
#
# Prerequisite: you already ran, and reviewed the output of,
#   gocov report -config CFG   and   gocov plan -config CFG
# and `claude -p` can authenticate (standalone CLI logged in, or ANTHROPIC_API_KEY).
#
# Example:
#   .\fanout.ps1 -Config C:\work\cfg.json -Repo shopapi -Pilot 3
param(
    [Parameter(Mandatory)][string]$Config,   # gocov config
    [Parameter(Mandatory)][string]$Repo,     # repo name from the config
    [int]$Pilot = 0,                         # only the first N queued items
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
$inv = [Globalization.CultureInfo]::InvariantCulture
$clock = [Diagnostics.Stopwatch]::StartNew()
$dryF = @(if ($DryRun) { "-dry-run" })

# Returns @{Model; Usd; Secs; Dir} for the next call, or $null with $nc = 4
# (budget/time limit reached: stop) or 5 (attempts used) and the reason in $why.
function Get-NextCall([string]$id, [int]$attempt) {
    # the parentheses matter: without them "+ $dryF" becomes two stray arguments and -dry-run is lost
    $out = G "next-call" (@("-item", $id, "-attempt", "$attempt", "-spent", $spent.ToString($inv), "-elapsed", "$([int]$clock.Elapsed.TotalSeconds)") + $dryF)
    $script:nc = $LASTEXITCODE
    $script:why = $out -join " "
    if ($script:nc -ne 0) { return $null }
    $f = $script:why -split ' ', 4
    return @{ Model = $f[0]; Usd = $f[1]; Secs = [int]$f[2]; Dir = $f[3] }
}

function Get-ClaudeLine($c, [string]$inFile, [string]$outFile) {
    # first claude.exe / claude.cmd on PATH (cmd.exe cannot run the extensionless npm shim)
    $exe = (Get-Command claude -CommandType Application | Where-Object { $_.Extension -in '.exe', '.cmd', '.bat' } | Select-Object -First 1).Source
    "`"$exe`" -p --output-format json --model $($c.Model) --permission-mode dontAsk --max-budget-usd $($c.Usd) --no-session-persistence --allowedTools `"$tools`" < `"$inFile`" > `"$outFile`""
}

# One capped call in the module dir, killed with its child processes after
# $c.Secs seconds so a hung call cannot block the run. Returns $true if killed
# (a killed call reports no cost; the caller charges its cap).
function Invoke-Claude($c, [string]$inFile, [string]$outFile) {
    $p = Start-Process -FilePath $env:ComSpec -ArgumentList "/d /s /c `"$(Get-ClaudeLine $c $inFile $outFile)`"" -WorkingDirectory $c.Dir -NoNewWindow -PassThru
    if ($p.WaitForExit($c.Secs * 1000)) { return $false }
    & taskkill /T /F /PID $p.Id | Out-Null
    Write-Host "[$Repo]   claude killed at the $($c.Secs)s timeout: charging its cap `$$($c.Usd) (a killed call reports no cost)"
    return $true
}

if (-not $DryRun) {
    & $gocov preflight -config $Config      # fail in seconds if claude -p cannot authenticate
    if ($LASTEXITCODE -ne 0) { exit 1 }
}

$work = New-Item -ItemType Directory -Path (Join-Path ([IO.Path]::GetTempPath()) ("fanout-" + [guid]::NewGuid()))
$spent = 0.0; $done = 0; $blocked = 0; $n = 0; $verified = $false; $nc = 0; $why = ""
try {
    $ids = @(G "queue")
    foreach ($id in $ids) {
        G "goal" | Out-Null
        if ($LASTEXITCODE -eq 0) {
            if ($DryRun) { Write-Host "goal already reached by estimate - nothing to dry-run"; exit 0 }
            Write-Host "goal reached by estimate - verifying with a fresh full run..."
            G "verify"; G "goal" | Out-Null
            if ($LASTEXITCODE -eq 0) { $verified = $true; break }
            Write-Host "verified below goal - the estimate was optimistic, continuing"
        }
        if ($Pilot -gt 0 -and $n -ge $Pilot) { Write-Host "pilot of $Pilot item(s) done"; break }
        $call = Get-NextCall $id 1
        if (-not $call) {
            if ($nc -eq 4) { Write-Host $why; break }   # remaining items stay queued
            Write-Host "gocov next-call failed ($nc): $why"; exit 1
        }
        $n++

        $begin = @(G "begin" (@("-item", $id, "-q") + $dryF))   # with -DryRun: saves nothing, leaves the tree alone
        $rc = $LASTEXITCODE
        if ($rc -eq 3) { Write-Host ($begin -join "`n"); $blocked++; continue }   # package does not build: blocked by gocov
        if ($rc -ne 0) { Write-Host "gocov begin failed ($rc) - stopping"; exit $rc }
        $promptFile = $begin[-1]
        $feedbackFile = $promptFile -replace '\.prompt\.md$', '.feedback.md'

        if ($DryRun) {
            Write-Host "--- DRY RUN: $id (in $($call.Dir), killed after $($call.Secs)s)"
            Write-Host (Get-ClaudeLine $call $promptFile "out.json")
            Write-Host ""; Get-Content $promptFile; exit 0
        }

        # attempt 1 uses the $call checked before begin: asking again could hit the
        # time limit during begin's measurement and block an item never attempted
        $ok = $false; $att = 1
        $in = Join-Path $work "in.md"; $out = Join-Path $work "out.json"
        while ($att -eq 1 -or ($call = Get-NextCall $id $att)) {
            Copy-Item $promptFile $in -Force
            if ((Test-Path $feedbackFile) -and (Get-Item $feedbackFile).Length -gt 0) {
                Add-Content $in "`n`nPREVIOUS ATTEMPT FEEDBACK - fix this first:`n$(Get-Content -Raw $feedbackFile)"
            }
            Remove-Item $out -ErrorAction SilentlyContinue
            Write-Host ("[{0}] {1} attempt {2} (model {3}, cap `${4}, timeout {5}s)" -f $Repo, $id, $att, $call.Model, $call.Usd, $call.Secs)
            $killed = Invoke-Claude $call $in $out
            $res = $null; try { $res = Get-Content -Raw $out | ConvertFrom-Json } catch {}
            $cost = if ($killed) { [double]::Parse($call.Usd, $inv) } elseif ($res -and $res.total_cost_usd) { [double]$res.total_cost_usd } else { 0.0 }
            $spent += $cost
            if ($res -and $res.is_error -and $cost -eq 0) {
                Write-Host "claude -p could not run: $($res.result)"
                Write-Host "(no model call was made; check login / API key, then re-run - the item stays queued)"; exit 1
            }
            G "finish" @("-item", $id, "-cost", $cost.ToString($inv))
            $rc = $LASTEXITCODE
            if ($rc -eq 0) { $ok = $true; $done++; break }
            if ($rc -ne 3) { Write-Host "gocov finish failed ($rc)"; exit $rc }
            $att++
        }
        if (-not $ok) {
            if ($nc -ne 4 -and $nc -ne 5) { Write-Host "gocov next-call failed ($nc): $why"; exit 1 }
            Write-Host $why; G "finish" @("-item", $id, "-abandon"); $blocked++
            if ($nc -eq 4) { break }   # budget gone mid-item: like gocov run, block it and stop
        }
        Write-Host ("[{0}] spent so far: `${1:N2}" -f $Repo, $spent)
    }
    if ($DryRun) { Write-Host "dry run: no item to show - nothing changed"; exit 0 }   # never verify/review on a dry run
    Write-Host ""; Write-Host ("== {0} done, {1} blocked, `${2:N2} spent." -f $done, $blocked, $spent)
    if (-not $verified) { Write-Host "Verifying with a fresh full run..."; G "verify" }
    if ($Review) { G "review" @("-headless") } else { Write-Host "Next: gocov review (adversarial review of the new tests)" }
} finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
