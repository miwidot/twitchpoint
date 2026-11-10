; TwitchPoint Windows installer (NSIS).
; Build:  makensis -DVERSION=2.4.1 -DEXE=..\..\twitchpoint-windows.exe installer\windows\twitchpoint.nsi
; Per-user install (no admin rights needed). The config lives in
; %APPDATA%\twitchpoint\config.json and is NEVER removed by the uninstaller.

Unicode true
!include "MUI2.nsh"

!ifndef VERSION
  !define VERSION "dev"
!endif
!ifndef EXE
  !define EXE "..\..\twitchpoint-windows.exe"
!endif

!define APPNAME "TwitchPoint"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\TwitchPoint"

Name "${APPNAME} ${VERSION}"
OutFile "twitchpoint-setup-${VERSION}.exe"
InstallDir "$LOCALAPPDATA\Programs\TwitchPoint"
InstallDirRegKey HKCU "Software\TwitchPoint" "InstallDir"
RequestExecutionLevel user
SetCompressor /SOLID lzma

!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\twitchpoint.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Start ${APPNAME} now"
!define MUI_FINISHPAGE_TEXT "${APPNAME} ${VERSION} has been installed on your computer.$\r$\n$\r$\nThe first start asks you to log in to Twitch.$\r$\n$\r$\nClick Finish to close Setup."

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Section "Install"
  SetOutPath "$INSTDIR"
  File "/oname=twitchpoint.exe" "${EXE}"

  ; Seed a default config ONLY if none exists (never overwrite user data).
  ; The app also creates this itself on first run; seeding here makes the
  ; file exist right after install.
  CreateDirectory "$APPDATA\twitchpoint"
  IfFileExists "$APPDATA\twitchpoint\config.json" skipconfig
    ; ClearErrors first: CreateDirectory above may have left the error flag
    ; set, which would make the IfErrors below skip seeding for no reason.
    ClearErrors
    FileOpen $0 "$APPDATA\twitchpoint\config.json" w
    IfErrors skipconfig
    FileWrite $0 '{$\r$\n'
    FileWrite $0 '  "auth_token": "",$\r$\n'
    FileWrite $0 '  "web_enabled": true,$\r$\n'
    FileWrite $0 '  "web_port": 8080,$\r$\n'
    FileWrite $0 '  "web_bind": "127.0.0.1",$\r$\n'
    FileWrite $0 '  "irc_enabled": true,$\r$\n'
    FileWrite $0 '  "drops_enabled": true,$\r$\n'
    FileWrite $0 '  "auto_claim": true,$\r$\n'
    FileWrite $0 '  "skip_badge_emote": false,$\r$\n'
    FileWrite $0 '  "skip_sub_rewards": false$\r$\n'
    FileWrite $0 '}$\r$\n'
    FileClose $0
  skipconfig:

  WriteUninstaller "$INSTDIR\uninstall.exe"
  CreateDirectory "$SMPROGRAMS\${APPNAME}"
  CreateShortcut "$SMPROGRAMS\${APPNAME}\${APPNAME}.lnk" "$INSTDIR\twitchpoint.exe"
  CreateShortcut "$SMPROGRAMS\${APPNAME}\Edit config.lnk" "notepad.exe" '"$APPDATA\twitchpoint\config.json"'
  CreateShortcut "$SMPROGRAMS\${APPNAME}\Uninstall.lnk" "$INSTDIR\uninstall.exe"

  WriteRegStr HKCU "Software\TwitchPoint" "InstallDir" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayName" "${APPNAME}"
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINSTKEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoRepair" 1
SectionEnd

Section "Uninstall"
  Delete "$INSTDIR\twitchpoint.exe"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${APPNAME}\*.lnk"
  RMDir "$SMPROGRAMS\${APPNAME}"
  DeleteRegKey HKCU "${UNINSTKEY}"
  DeleteRegKey HKCU "Software\TwitchPoint"
  ; Config in %APPDATA%\twitchpoint is intentionally kept.
SectionEnd
