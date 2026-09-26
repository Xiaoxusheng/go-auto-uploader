@echo off
chcp 65001 >nul
title 连续训练守护
powershell -NoProfile -ExecutionPolicy Bypass -File "D:\upload\continuous_training.ps1"
pause
