import pytest

from calc import add


def test_add() -> None:
    assert add(2, 3) == 5


@pytest.mark.integration
def test_add_integration() -> None:
    assert add(add(1, 2), 3) == 6
