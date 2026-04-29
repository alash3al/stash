#Requires -Version 5.1
<#
.SYNOPSIS
    Interactively configure Stash's Ollama model settings in .env.

.DESCRIPTION
    Detects available Ollama models, prompts you to select a reasoning model,
    auto-detects an embedding model, then updates the local .env file.
    Portable: resolves .env relative to this script's location.

.EXAMPLE
    .\set-ollama-models.ps1
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Resolve .env path relative to this script — portable across machines.
# ---------------------------------------------------------------------------
$EnvPath = Join-Path $PSScriptRoot '.env'

# ---------------------------------------------------------------------------
# Embedding dimension lookup table.
# Add new model families here as they become available.
# ---------------------------------------------------------------------------
$DimTable = [ordered]@{
    'nomic'  = 768
    'bge'    = 1024
    'mxbai'  = 1024
    'e5'     = 1024
    'jina'   = 768
    'gte'    = 1024
    # OpenAI-compatible fallback
    'ada'    = 1536
    'small'  = 1536
    'large'  = 3072
}

function Get-EmbeddingDim {
    param([string]$ModelName)
    $name = $ModelName.ToLower()
    foreach ($key in $DimTable.Keys) {
        if ($name -like "*$key*") { return $DimTable[$key] }
    }
    return 1536  # Safe OpenAI-compatible fallback
}

function Test-OllamaRunning {
    try {
        $null = Invoke-RestMethod -Uri 'http://localhost:11434/api/tags' -TimeoutSec 3
        return $true
    } catch {
        return $false
    }
}

function Update-StashConfig {
    Write-Host ''
    Write-Host '==========================================' -ForegroundColor Cyan
    Write-Host '   Stash — Ollama Model Configurator'      -ForegroundColor Cyan
    Write-Host '==========================================' -ForegroundColor Cyan
    Write-Host ''

    # --- Pre-flight: confirm .env exists ---
    if (-not (Test-Path $EnvPath)) {
        Write-Host "ERROR: .env not found at: $EnvPath" -ForegroundColor Red
        Write-Host 'Copy .env.example to .env and try again.' -ForegroundColor Yellow
        exit 1
    }

    # --- Pre-flight: confirm Ollama is reachable ---
    Write-Host 'Checking Ollama connectivity...' -ForegroundColor Gray
    if (-not (Test-OllamaRunning)) {
        Write-Host 'ERROR: Ollama is not running or not reachable at http://localhost:11434' -ForegroundColor Red
        Write-Host 'Start Ollama and try again.' -ForegroundColor Yellow
        exit 1
    }
    Write-Host 'Ollama is running.' -ForegroundColor Green
    Write-Host ''

    # --- Fetch model list ---
    $ollamaOutput = ollama list
    $models = $ollamaOutput | Select-Object -Skip 1 | Where-Object { $_.Trim() -ne '' } | ForEach-Object {
        $parts = $_ -split '\s+'
        if ($parts.Count -ge 3) {
            [PSCustomObject]@{
                Name = $parts[0]
                Size = $parts[2]
            }
        }
    }

    if (-not $models -or $models.Count -eq 0) {
        Write-Host 'ERROR: No Ollama models found. Pull a model first:' -ForegroundColor Red
        Write-Host '  ollama pull llama3.1:8b' -ForegroundColor Yellow
        exit 1
    }

    # --- Select reasoning model ---
    Write-Host 'Available Models:' -ForegroundColor Yellow
    for ($i = 0; $i -lt $models.Count; $i++) {
        Write-Host "  [$i] $($models[$i].Name)  ($($models[$i].Size))"
    }
    Write-Host ''

    $defaultReasoner = 'llama3.1:8b'
    $choice = Read-Host "Select reasoning model index (Enter for default: $defaultReasoner)"

    if ([string]::IsNullOrWhiteSpace($choice)) {
        $reasoner = $defaultReasoner
    } else {
        $idx = [int]$choice
        if ($idx -lt 0 -or $idx -ge $models.Count) {
            Write-Host "ERROR: Invalid selection '$choice'." -ForegroundColor Red
            exit 1
        }
        $reasoner = $models[$idx].Name
    }
    Write-Host "Reasoning model : $reasoner" -ForegroundColor Green

    # --- Auto-detect embedding model ---
    $embedder = $models | Where-Object { $_.Name -match 'embed|nomic|bge|mxbai|e5|jina|gte' } | Select-Object -First 1

    if ($embedder) {
        $embedderName = $embedder.Name
        $dim          = Get-EmbeddingDim -ModelName $embedderName
        Write-Host "Embedding model : $embedderName (dim $dim) — auto-detected" -ForegroundColor Green
    } else {
        Write-Host 'WARNING: No dedicated embedding model detected.' -ForegroundColor Yellow
        Write-Host "Falling back to reasoning model for embeddings (slower, less accurate)." -ForegroundColor Gray
        $embedderName = $reasoner
        $dim          = Get-EmbeddingDim -ModelName $embedderName
        Write-Host "Embedding model : $embedderName (dim $dim) — fallback" -ForegroundColor Gray
    }

    Write-Host ''

    # --- Dry-run confirmation ---
    Write-Host 'The following changes will be written to .env:' -ForegroundColor Cyan
    Write-Host "  STASH_REASONER_MODEL  = $reasoner"
    Write-Host "  STASH_EMBEDDING_MODEL = $embedderName"
    Write-Host "  STASH_VECTOR_DIM      = $dim"
    Write-Host ''

    $confirm = Read-Host 'Apply changes? [Y/n]'
    if ($confirm -match '^[Nn]') {
        Write-Host 'Aborted. No changes made.' -ForegroundColor Yellow
        exit 0
    }

    # --- Apply changes ---
    $timestamp = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
    $content   = Get-Content $EnvPath -Raw

    $content = $content -replace '(?m)^STASH_REASONER_MODEL=.*',  "STASH_REASONER_MODEL=$reasoner"
    $content = $content -replace '(?m)^STASH_EMBEDDING_MODEL=.*', "STASH_EMBEDDING_MODEL=$embedderName"
    $content = $content -replace '(?m)^STASH_VECTOR_DIM=.*',      "STASH_VECTOR_DIM=$dim"

    # Append a last-updated comment at end if not already present, else replace it.
    $content = $content -replace '(?m)^# Last configured by set-ollama-models\.ps1:.*(\r?\n)?', ''
    $content = $content.TrimEnd() + "`n# Last configured by set-ollama-models.ps1: $timestamp`n"

    Set-Content -Path $EnvPath -Value $content -NoNewline

    Write-Host ''
    Write-Host ".env updated successfully ($timestamp)" -ForegroundColor Green
    Write-Host ''
}

Update-StashConfig
