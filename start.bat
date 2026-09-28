@echo off
REM EdgeLite Gateway (Go Edition) - Windows Startup Script
REM Usage: start.bat [--host HOST] [--port PORT] [--config CONFIG_PATH]

setlocal

REM Default values
set HOST=0.0.0.0
set PORT=8080
set CONFIG=configs/config.yaml

REM Check for environment variable overrides
if not "%EDGELITE_SERVER__HOST%"=="" set HOST=%EDGELITE_SERVER__HOST%
if not "%EDGELITE_SERVER__PORT%"=="" set PORT=%EDGELITE_SERVER__PORT%
if not "%EDGELITE_CONFIG%"=="" set CONFIG=%EDGELITE_CONFIG%

REM Parse command line arguments
:parse
if "%1"=="--host" (
    set HOST=%2
    shift
    shift
    goto parse
)
if "%1"=="--port" (
    set PORT=%2
    shift
    shift
    goto parse
)
if "%1"=="--config" (
    set CONFIG=%2
    shift
    shift
    goto parse
)
if "%1"=="--help" goto help
if "%1"=="-h" goto help
if "%1"=="" goto run

:help
echo EdgeLite Gateway (Go Edition)
echo.
echo Usage: start.bat [OPTIONS]
echo.
echo Options:
echo   --host HOST       Listen address (default: 0.0.0.0)
echo   --port PORT       Listen port (default: 8080)
echo   --config PATH     Config file (default: configs/config.yaml)
echo   --help, -h        Show this help
goto end

:run
REM Check if binary exists, build if not
if not exist "edgelite.exe" (
    if not exist "edgelite" (
        echo Building EdgeLite Gateway...
        go build -o edgelite.exe ./cmd/edgelite
    )
)

REM Run the appropriate binary
if exist "edgelite.exe" (
    echo Starting EdgeLite Gateway...
    echo   Host: %HOST%
    echo   Port: %PORT%
    echo   Config: %CONFIG%
    echo.
    edgelite.exe --host %HOST% --port %PORT% --config %CONFIG%
) else if exist "edgelite" (
    echo Starting EdgeLite Gateway...
    echo   Host: %HOST%
    echo   Port: %PORT%
    echo   Config: %CONFIG%
    echo.
    edgelite --host %HOST% --port %PORT% --config %CONFIG%
) else (
    echo ERROR: Binary not found. Run 'go build -o edgelite.exe ./cmd/edgelite' first.
    exit /b 1
)

:end
endlocal
