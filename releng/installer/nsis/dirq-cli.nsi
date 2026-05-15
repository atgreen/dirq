; DirQ CLI - Windows Installer
; NSIS Script
;
; SPDX-License-Identifier: MIT
; Copyright (C) 2026 Anthony Green <green@moxielogic.com>

!include "MUI2.nsh"
!include "FileFunc.nsh"

; ============================================================================
; General Configuration
; ============================================================================

!define PRODUCT_NAME "DirQ CLI"
!define PRODUCT_SHORT "DirQ"
!define PRODUCT_PUBLISHER "Anthony Green"
!define PRODUCT_WEB_SITE "https://github.com/atgreen/dirq"
!define PRODUCT_UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_SHORT}CLI"
!define PRODUCT_UNINST_ROOT_KEY "HKLM"

; Version is passed from command line: makensis /DVERSION=0.5.0 dirq-cli.nsi
!ifndef VERSION
  !define VERSION "0.0.0"
!endif

Name "${PRODUCT_NAME} ${VERSION}"
OutFile "dirq-cli-${VERSION}-setup.exe"
InstallDir "$PROGRAMFILES64\DirQ"
InstallDirRegKey HKLM "Software\${PRODUCT_SHORT}" "InstallDir"
RequestExecutionLevel admin
SetCompressor /SOLID lzma

; ============================================================================
; Modern UI Configuration
; ============================================================================

!define MUI_ABORTWARNING
!define MUI_ICON "${NSISDIR}\Contrib\Graphics\Icons\modern-install.ico"
!define MUI_UNICON "${NSISDIR}\Contrib\Graphics\Icons\modern-uninstall.ico"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "..\..\..\LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!define MUI_FINISHPAGE_TEXT "${PRODUCT_NAME} has been installed.$\r$\n$\r$\nAdd $INSTDIR to your PATH to use dirq from any terminal."
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

; ============================================================================
; Installer Section
; ============================================================================

Section "DirQ CLI" SEC_CORE
  SectionIn RO

  ; Install binary and connection plugin.
  SetOutPath "$INSTDIR"
  File "..\..\..\bin\dirq.exe"
  File "..\..\..\LICENSE"

  SetOutPath "$INSTDIR\connection_plugins"
  File "..\..\..\ansible\connection_plugins\dirq.py"

  ; Store installation folder.
  WriteRegStr HKLM "Software\${PRODUCT_SHORT}" "InstallDir" "$INSTDIR"

  ; Add to PATH.
  EnVar::AddValue "PATH" "$INSTDIR"

  ; Create uninstaller.
  WriteUninstaller "$INSTDIR\uninstall-cli.exe"

  ; Add to Add/Remove Programs.
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "DisplayName" "${PRODUCT_NAME}"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "Publisher" "${PRODUCT_PUBLISHER}"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "URLInfoAbout" "${PRODUCT_WEB_SITE}"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "UninstallString" "$INSTDIR\uninstall-cli.exe"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegDWORD ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "NoModify" 1
  WriteRegDWORD ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "NoRepair" 1

  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  IntFmt $0 "0x%08X" $0
  WriteRegDWORD ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "EstimatedSize" "$0"
SectionEnd

; ============================================================================
; Uninstaller Section
; ============================================================================

Section "Uninstall"
  ; Remove from PATH.
  EnVar::DeleteValue "PATH" "$INSTDIR"

  ; Remove files.
  Delete "$INSTDIR\dirq.exe"
  Delete "$INSTDIR\LICENSE"
  Delete "$INSTDIR\connection_plugins\dirq.py"
  RMDir "$INSTDIR\connection_plugins"
  Delete "$INSTDIR\uninstall-cli.exe"
  RMDir "$INSTDIR"

  ; Remove registry keys.
  DeleteRegKey ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}"
SectionEnd
