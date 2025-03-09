import pyautogui
import cv2
import numpy as np
import time
import os

# Function to find and click an image
def find_and_click_image(image_path, threshold=0.8):
    # Load the template image
    template = cv2.imread(image_path, cv2.IMREAD_GRAYSCALE)

    # Take a screenshot of the screen
    screenshot = pyautogui.screenshot()
    screenshot = np.array(screenshot)
    screenshot_gray = cv2.cvtColor(screenshot, cv2.COLOR_RGB2GRAY)

    # Match the template image to the screenshot
    result = cv2.matchTemplate(screenshot_gray, template, cv2.TM_CCOEFF_NORMED)

    # Get the location of the match (highest correlation)
    locations = np.where(result >= threshold)

    if len(locations[0]) > 0:
        # Get the first match location
        match_location = (locations[1][0], locations[0][0])

        # Calculate the center of the matched area
        match_center = pyautogui.center(match_location)

        # Move the mouse and click
        pyautogui.click(match_center)
        print(f"Clicked on image at {match_center}")
        return True
    else:
        print(f"Image '{image_path}' not found on the screen.")
        return False

# Automatically find all JPG/JPEG files in the same directory as the script
def find_jpeg_files_in_script_directory():
    # Get the current working directory (where the script is located)
    current_directory = os.path.dirname(os.path.abspath(__file__))
    
    # List all JPG/JPEG files in the current directory
    return [f for f in os.listdir(current_directory) if f.endswith('.jpg') or f.endswith('.jpeg')]

# Get all JPG/JPEG files in the same directory as the script
jpeg_files = find_jpeg_files_in_script_directory()

# Loop to check and click images every 5 seconds
while True:
    for jpeg_file in jpeg_files:
        image_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), jpeg_file)
        find_and_click_image(image_path)

    # Wait for 5 seconds before repeating
    time.sleep(5)
